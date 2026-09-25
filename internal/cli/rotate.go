package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/sopsx"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newRotateCACmd() *cobra.Command {
	var (
		talos      bool
		kubernetes bool
		dryRun     bool
		yes        bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "rotate-ca",
		Short: "Rotate the cluster's Talos and Kubernetes API CAs, and the secrets bundle with them",
		Long: `Rotate-ca runs "talosctl rotate-ca" against every node in the config, which
generates new root CAs for the Talos API and the Kubernetes API and rolls them
out gracefully: the new CA is accepted everywhere before anything is issued
from it, and the old one is dropped last.

That leaves the secrets bundle holding the old CAs, and the next "talman
apply" would put them back. So once the rotation has finished, talman reads a
control plane's machine config, extracts the bundle from it as
"talman secrets generate --from-controlplane-config" does, and writes it over
secrets.sops.yaml -- encrypted if the old one was. The old bundle is kept
beside it, and the talosconfig is replaced with the one signed by the new CA.

--dry-run asks talosctl what it would do and changes nothing. A real rotation
asks for the cluster name first, unless --yes.

Afterwards, commit the new bundle, run "talman render", and fetch a new
"talman kubeconfig" if the Kubernetes CA was rotated.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rec := currentRun

			if !talos && !kubernetes {
				return errors.New("--talos=false and --kubernetes=false leave nothing to rotate")
			}

			if dryRun {
				rec.SetLabel("dry_run", "true")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			old, err := os.ReadFile(cfg.SecretPath())
			if err != nil {
				return fmt.Errorf("reading the secrets bundle a rotation has to replace: %w", err)
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			// Left by a rotation that did not finish, this may be the only
			// talosconfig the cluster still accepts. It is never removed
			// here: deleting it could lock the operator out of every node.
			rotated := tc + ".rotated"
			if exists(rotated) {
				return fmt.Errorf("%s is left from a rotation that did not finish, and may be the only "+
					"talosconfig the cluster still accepts: if `talosctl --talosconfig %s health` passes, "+
					"move it over %s; delete it only if you are sure it is not needed; then run rotate-ca again",
					render.Rel(rotated), render.Rel(rotated), render.Rel(tc))
			}

			// Whether the new bundle can be written the way the old one is,
			// before anything is rotated: finding out afterwards leaves a
			// cluster trusting CAs no bundle holds.
			if !dryRun && sopsx.IsEncrypted(old) {
				if _, err := sopsx.EncryptTo([]byte("preflight: true\n"), cfg.SecretPath()); err != nil {
					return fmt.Errorf("%s is encrypted, and the rotated bundle could not be encrypted "+
						"the same way, so nothing was rotated: %w", cfg.SecretFile, err)
				}
			}

			if !dryRun && !yes {
				which := map[[2]bool]string{
					{true, true}:  "the Talos API and Kubernetes API CAs",
					{true, false}: "the Talos API CA",
					{false, true}: "the Kubernetes API CA",
				}[[2]bool{talos, kubernetes}]

				if err := typeClusterName("rotate-ca", cfg.ClusterName, fmt.Sprintf(
					"About to rotate %s of cluster %q across %d node(s), and replace %s.",
					which, cfg.ClusterName, len(cfg.Nodes), cfg.SecretFile)); err != nil {
					return err
				}
			}

			var cps, workers []string

			for i := range cfg.Nodes {
				if cfg.Nodes[i].IsControlPlane() {
					cps = append(cps, cfg.Nodes[i].IPAddress)
				} else {
					workers = append(workers, cfg.Nodes[i].IPAddress)
				}
			}

			// A dry run's talosconfig goes somewhere private and is thrown
			// away: only a real rotation's is worth keeping.
			output := rotated

			if dryRun {
				stage, cleanup, err := interrupt.TempDir("", "talman-rotate-")
				if err != nil {
					return err
				}
				defer cleanup()

				output = filepath.Join(stage, "talosconfig")
			}

			args := []string{
				"--talosconfig", tc,
				"rotate-ca",
				"--control-plane-nodes", strings.Join(cps, ","),
				fmt.Sprintf("--talos=%t", talos),
				fmt.Sprintf("--kubernetes=%t", kubernetes),
				fmt.Sprintf("--dry-run=%t", dryRun),
				"--output", output,
				// talman's renders carry neither, and the next apply would
				// only take them out again.
				"--with-docs=false",
				"--with-examples=false",
			}

			if len(workers) > 0 {
				args = append(args, "--worker-nodes", strings.Join(workers, ","))
			}

			tal := runner(cfg)

			if err := tal.Stream(append(args, extraFlags...)...); err != nil {
				if dryRun {
					return err
				}

				// talosctl stopped partway, and how far it got is not
				// something talman can tell from outside: it rotates the
				// Talos CA first and writes the talosconfig for the new
				// one when that is done, so that file is the one clue.
				return fmt.Errorf("%w\n  the rotation did not finish, and may have got partway.%s",
					err, partialRotation(cfg, tc, rotated))
			}

			if dryRun {
				fmt.Fprintln(os.Stderr, "dry run: nothing was rotated")

				return nil
			}

			rec.SetChanged(true)

			// From here the cluster trusts the new CAs and the bundle does
			// not, so a failure has to say how to finish by hand.
			talosconfig := tc
			if talos {
				talosconfig = rotated
			}

			if err := replaceBundle(cfg, tal, talosconfig, old); err != nil {
				return fmt.Errorf("%w\n  the cluster's CAs WERE rotated, but %s still holds the old ones.\n%s",
					err, cfg.SecretFile, finishByHand(cfg, talosconfig))
			}

			if talos {
				if err := os.Rename(rotated, tc); err != nil {
					return fmt.Errorf("replacing %s with the rotated talosconfig at %s: %w",
						render.Rel(tc), render.Rel(rotated), err)
				}

				fmt.Fprintf(os.Stderr, "wrote %s, signed by the new Talos CA\n", render.Rel(tc))
			}

			fmt.Fprintln(os.Stderr, "next:")

			if sopsx.IsEncrypted(old) {
				fmt.Fprintf(os.Stderr, "  commit %s\n", cfg.SecretFile)
			} else {
				fmt.Fprintf(os.Stderr, "  keep %s out of git: it is unencrypted\n", cfg.SecretFile)
			}

			fmt.Fprintln(os.Stderr, "  talman render       the rendered configs still carry the old CAs")

			if kubernetes {
				fmt.Fprintln(os.Stderr, "  talman kubeconfig   the old one is signed by the old Kubernetes CA")
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&talos, "talos", true, "rotate the Talos API CA")
	cmd.Flags().BoolVar(&kubernetes, "kubernetes", true, "rotate the Kubernetes API CA")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be rotated, and change nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// replaceBundle extracts the secrets bundle from a control plane's current
// machine config and writes it over the configured one, in the same form --
// encrypted or not -- keeping the old bundle beside it.
func replaceBundle(cfg *config.Config, tal *talosctl.Runner, talosconfig string, old []byte) error {
	from, err := healthNode(cfg, tal, talosconfig)
	if err != nil {
		return err
	}

	machineConfig, err := tal.Output(append(tal.NodeArgs(talosconfig, from.IPAddress),
		"read", "/system/state/config.yaml")...)
	if err != nil {
		return fmt.Errorf("reading %s's machine config: %w", from.Hostname, err)
	}

	// The machine config carries every key the bundle does, so it is staged
	// like the decrypted bundle is: private, and gone after the run.
	stage, cleanup, err := interrupt.TempDir("", "talman-rotate-")
	if err != nil {
		return err
	}
	defer cleanup()

	staged := filepath.Join(stage, "controlplane.yaml")
	if err := os.WriteFile(staged, machineConfig, 0o600); err != nil {
		return err
	}

	bundle, err := tal.GenSecrets(talosctl.GenSecretsOptions{
		TalosVersion:           cfg.TalosVersion,
		FromControlPlaneConfig: staged,
	})
	if err != nil {
		return err
	}

	dest := cfg.SecretPath()

	if sopsx.IsEncrypted(old) {
		if bundle, err = sopsx.EncryptTo(bundle, dest); err != nil {
			return err
		}
	}

	// Into the output directory, gitignored: a plaintext bundle's copy still
	// holds every key rotate-ca does not change -- etcd's, the service
	// account's, the cluster secret -- and beside the bundle it would be one
	// `git add .` from a commit.
	if err := render.PrepareOutput(cfg, os.Stderr); err != nil {
		return err
	}

	backup := filepath.Join(cfg.OutputPath(),
		fmt.Sprintf("secrets-pre-rotate-%s.yaml", time.Now().UTC().Format("20060102T150405Z")))
	if err := render.WriteAtomic(backup, old); err != nil {
		return fmt.Errorf("keeping the old bundle: %w", err)
	}

	if err := render.WriteAtomic(dest, bundle); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wrote %s, extracted from %s (the old one is %s)\n",
		cfg.SecretFile, from.Hostname, render.Rel(backup))

	return nil
}

// partialRotation says what an unfinished rotation left behind, and how to
// finish it.
func partialRotation(cfg *config.Config, tc, rotated string) string {
	if !exists(rotated) {
		return fmt.Sprintf("\n  talosctl wrote no talosconfig for a new Talos CA, so %s is still the one to use,\n"+
			"  and %s still matches the Talos CA. Check the cluster with `talman health`, then run\n"+
			"  `talman rotate-ca` again; if the Kubernetes CA got partway, the rerun carries it through.",
			render.Rel(tc), cfg.SecretFile)
	}

	return fmt.Sprintf("\n  talosctl wrote %s, the talosconfig for a new Talos CA, so the Talos CA has\n"+
		"  probably been rotated and %s may no longer be accepted. Once `talosctl --talosconfig %s health`\n"+
		"  passes, make it talman's and bring %s in line:\n"+
		"    mv %s %s\n%s",
		render.Rel(rotated), render.Rel(tc), render.Rel(rotated), cfg.SecretFile,
		render.Rel(rotated), render.Rel(tc), finishByHand(cfg, tc))
}

// finishByHand spells out extracting the bundle from a control plane, which is
// what rotate-ca does itself once a rotation has finished.
func finishByHand(cfg *config.Config, talosconfig string) string {
	return fmt.Sprintf("  finish by hand before the next apply:\n"+
		"    talosctl --talosconfig %s --nodes <control plane> read /system/state/config.yaml > cp.yaml\n"+
		"    talman secrets generate --force --from-controlplane-config cp.yaml    # rewrites %s\n"+
		"    rm cp.yaml    # it holds every key in the bundle",
		render.Rel(talosconfig), cfg.SecretFile)
}
