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

			rotated := tc + ".rotated"
			_ = os.Remove(rotated)

			args := []string{
				"--talosconfig", tc,
				"rotate-ca",
				"--control-plane-nodes", strings.Join(cps, ","),
				fmt.Sprintf("--talos=%t", talos),
				fmt.Sprintf("--kubernetes=%t", kubernetes),
				fmt.Sprintf("--dry-run=%t", dryRun),
				"--output", rotated,
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
				return err
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
				return fmt.Errorf("%w\n  the cluster's CAs WERE rotated, but %s still holds the old ones; "+
					"finish by hand before the next apply:\n"+
					"    talosctl --talosconfig %s --nodes <control plane> read /system/state/config.yaml > cp.yaml\n"+
					"    talman secrets generate --force --from-controlplane-config cp.yaml\n"+
					"    rm cp.yaml",
					err, cfg.SecretFile, render.Rel(talosconfig))
			}

			if talos {
				if err := os.Rename(rotated, tc); err != nil {
					return fmt.Errorf("replacing %s with the rotated talosconfig at %s: %w",
						render.Rel(tc), render.Rel(rotated), err)
				}

				fmt.Fprintf(os.Stderr, "wrote %s, signed by the new Talos CA\n", render.Rel(tc))
			}

			fmt.Fprintln(os.Stderr, "next:")
			fmt.Fprintf(os.Stderr, "  commit %s\n", cfg.SecretFile)
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

	machineConfig, err := tal.Output("--talosconfig", talosconfig, "--nodes", from.IPAddress,
		"read", "/system/state/config.yaml")
	if err != nil {
		return fmt.Errorf("reading %s's machine config: %w", from.Hostname, err)
	}

	// The machine config carries every key the bundle does, so it is staged
	// like the decrypted bundle is: private, and gone after the run.
	stage, err := os.MkdirTemp("", "talman-rotate-")
	if err != nil {
		return err
	}

	defer os.RemoveAll(stage) //nolint:errcheck // best effort cleanup; the error that matters is returned below
	defer interrupt.RemoveAllOnExit(stage)()

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

	backup := fmt.Sprintf("%s.pre-rotate-%s", dest, time.Now().UTC().Format("20060102T150405Z"))
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
