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
		finish     bool
		yes        bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "rotate-ca",
		Short: "Rotate the cluster's Talos and Kubernetes API CAs, and the secrets bundle with them",
		Long: `Rotate-ca rotates the Talos API and Kubernetes API CAs across every node, then
rebuilds the secrets bundle from a control plane so the next apply does not
put the old CAs back. The old bundle is kept in the output directory, and the
talosconfig is replaced with one signed by the new CA.

--dry-run shows what talosctl would do. A real rotation asks for the cluster
name first, unless --yes, and refuses to start unless the bundle can be read
back from a control plane. --finish completes a rotation that stopped after
the nodes changed. Afterwards: commit the bundle, run "talman render", and
"talman kubeconfig" if the Kubernetes CA changed.

More in the README: "Rotating the CAs".`,
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

			tal := runner(cfg)

			// Left by a rotation that did not finish, this may be the only
			// talosconfig the cluster still accepts. It is never removed
			// here: deleting it could lock the operator out of every node.
			rotated := tc + ".rotated"

			// The second half alone, for a rotation that finished on the
			// nodes but not here: the bundle still holds the old CAs.
			if finish {
				// It replaces the bundle and the talosconfig, so it is
				// asked about like a rotation, and has no dry run to offer:
				// what it does is read and write, nothing to preview.
				if dryRun {
					return errors.New("--finish replaces the bundle with the one the cluster runs, and has " +
						"no dry run: `talman status` shows the cluster, and the bundle's old copy is kept")
				}

				if !yes {
					if err := typeClusterName("rotate-ca --finish", cfg.ClusterName, fmt.Sprintf(
						"About to replace %s with the bundle cluster %q runs, and the talosconfig with %s if "+
							"there is one.", cfg.SecretFile, cfg.ClusterName, render.Rel(rotated))); err != nil {
						return err
					}
				}

				return finishRotation(cfg, tal, tc, rotated, old)
			}

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

			// Whether the bundle can be read back out of the cluster, before
			// anything is rotated: finding out afterwards leaves a cluster
			// trusting CAs no bundle holds.
			if !dryRun {
				if _, _, err := extractBundle(cfg, tal, tc, 0); err != nil {
					return fmt.Errorf("%w\n  talman reads the rotated bundle back from a control plane, and could "+
						"not read this one, so nothing was rotated", err)
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

			// talosctl rotate-ca talks to the cluster through exactly one
			// node, and reaches the rest by the two lists below. Left to
			// itself it takes the talosconfig's default nodes -- every node,
			// as render writes it -- and refuses to start. So one control
			// plane is picked the way the health check picks one: the first
			// that answers, reached the way it answered.
			from, err := healthNode(cfg, tal, tc)
			if err != nil {
				return err
			}

			args := append(tal.NodeArgs(tc, from.IPAddress),
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
			)

			if len(workers) > 0 {
				args = append(args, "--worker-nodes", strings.Join(workers, ","))
			}

			if err := tal.Stream(append(args, extraFlags...)...); err != nil {
				if dryRun {
					return err
				}

				// Made whole even so: it may be the only talosconfig the
				// cluster still accepts.
				if exists(rotated) {
					_ = completeTalosconfig(tal, cfg, rotated)
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

			if talos {
				if err := completeTalosconfig(tal, cfg, rotated); err != nil {
					return fmt.Errorf("the CAs WERE rotated, and %s is the talosconfig for them, but setting its "+
						"endpoints and nodes failed: %w", render.Rel(rotated), err)
				}
			}

			// From here the cluster trusts the new CAs and the bundle does
			// not. The control planes may still be settling, so the read is
			// given a couple of minutes before --finish is the way on.
			talosconfig := tc
			if talos {
				talosconfig = rotated
			}

			if err := replaceBundle(cfg, tal, talosconfig, old, 2*time.Minute); err != nil {
				return fmt.Errorf("%w\n  the cluster's CAs WERE rotated, but %s still holds the old ones; "+
					"once the cluster answers again, finish with:\n    %s", err, cfg.SecretFile,
					talmanCmd("rotate-ca --finish"))
			}

			if talos {
				if err := os.Rename(rotated, tc); err != nil {
					return fmt.Errorf("replacing %s with the rotated talosconfig at %s: %w",
						render.Rel(tc), render.Rel(rotated), err)
				}

				fmt.Fprintf(os.Stderr, "wrote %s, signed by the new Talos CA\n", render.Rel(tc))
			}

			printRotationNext(cfg, old, kubernetes)

			return nil
		},
	}

	cmd.Flags().BoolVar(&talos, "talos", true, "rotate the Talos API CA")
	cmd.Flags().BoolVar(&kubernetes, "kubernetes", true, "rotate the Kubernetes API CA")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "say what would be rotated, and change nothing")
	cmd.Flags().BoolVar(&finish, "finish", false,
		"rotate nothing: bring the bundle and talosconfig in line with a cluster whose CAs were rotated")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// extractBundle reads a control plane's running machine config and extracts
// the secrets bundle from it, as `talman secrets generate
// --from-controlplane-config` does, trying for up to patience: just after a
// rotation the control planes may still be settling on the new CA.
func extractBundle(cfg *config.Config, tal *talosctl.Runner, talosconfig string,
	patience time.Duration,
) ([]byte, *config.Node, error) {
	deadline := time.Now().Add(patience)

	for {
		bundle, from, err := extractBundleOnce(cfg, tal, talosconfig)
		if err == nil || !time.Now().Before(deadline) {
			return bundle, from, err
		}

		if err := interrupt.Sleep(5 * time.Second); err != nil {
			return nil, nil, err
		}
	}
}

func extractBundleOnce(cfg *config.Config, tal *talosctl.Runner, talosconfig string) ([]byte, *config.Node, error) {
	from, err := healthNode(cfg, tal, talosconfig)
	if err != nil {
		return nil, nil, err
	}

	machineConfig, err := tal.MachineConfig(talosconfig, from.IPAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s's machine config: %w", from.Hostname, err)
	}

	// The machine config carries every key the bundle does, so it is staged
	// like the decrypted bundle is: private, and gone after the run.
	stage, cleanup, err := interrupt.TempDir("", "talman-rotate-")
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	staged := filepath.Join(stage, "controlplane.yaml")
	if err := os.WriteFile(staged, machineConfig, 0o600); err != nil {
		return nil, nil, err
	}

	bundle, err := tal.GenSecrets(talosctl.GenSecretsOptions{
		TalosVersion:           cfg.TalosVersion,
		FromControlPlaneConfig: staged,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("extracting the bundle from %s's machine config: %w", from.Hostname, err)
	}

	return bundle, from, nil
}

// replaceBundle writes the bundle a control plane's running config holds over
// the configured one, in the same form -- encrypted or not -- keeping the old
// one in the output directory.
func replaceBundle(cfg *config.Config, tal *talosctl.Runner, talosconfig string, old []byte,
	patience time.Duration,
) error {
	bundle, from, err := extractBundle(cfg, tal, talosconfig, patience)
	if err != nil {
		return err
	}

	dest := cfg.SecretPath()

	if sopsx.IsEncrypted(old) {
		if bundle, err = sopsx.EncryptTo(bundle, dest); err != nil {
			return err
		}
	}

	backup, err := keepBundle(cfg, old, "pre-rotate")
	if err != nil {
		return err
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
			"  `%s` again; if the Kubernetes CA got partway, the rerun carries it through.",
			render.Rel(tc), cfg.SecretFile, talmanCmd("rotate-ca"))
	}

	return fmt.Sprintf("\n  talosctl wrote %s, the talosconfig for a new Talos CA, so the Talos CA has\n"+
		"  probably been rotated and %s may no longer be accepted. Once\n"+
		"  `talosctl --talosconfig %s health` passes, finish with:\n"+
		"    "+talmanCmd("rotate-ca --finish"),
		render.Rel(rotated), render.Rel(tc), render.Rel(rotated))
}

// completeTalosconfig gives the talosconfig talosctl rotate-ca wrote the
// endpoints and default nodes render gives talman's own.
//
// talosctl writes it with the endpoints the rotating client had -- the one
// control plane it was pinned to -- and no nodes at all, so once it replaced
// talman's, plain talosctl, or `talman ctl` without -n, failed with "nodes
// are not set" until the next render.
func completeTalosconfig(tal *talosctl.Runner, cfg *config.Config, path string) error {
	cps := make([]string, 0, len(cfg.Nodes))
	all := make([]string, 0, len(cfg.Nodes))

	for i := range cfg.Nodes {
		all = append(all, cfg.Nodes[i].IPAddress)

		if cfg.Nodes[i].IsControlPlane() {
			cps = append(cps, cfg.Nodes[i].IPAddress)
		}
	}

	if err := tal.ConfigEndpoint(path, cps); err != nil {
		return err
	}

	return tal.ConfigNode(path, all)
}

// finishRotation brings the bundle, and the talosconfig when talosctl left a
// rotated one, in line with a cluster whose CAs were rotated.
func finishRotation(cfg *config.Config, tal *talosctl.Runner, tc, rotated string, old []byte) error {
	talosconfig := tc

	if exists(rotated) {
		if err := completeTalosconfig(tal, cfg, rotated); err != nil {
			return err
		}

		talosconfig = rotated
	}

	if err := replaceBundle(cfg, tal, talosconfig, old, 0); err != nil {
		return err
	}

	if talosconfig == rotated {
		if err := os.Rename(rotated, tc); err != nil {
			return fmt.Errorf("replacing %s with the rotated talosconfig at %s: %w",
				render.Rel(tc), render.Rel(rotated), err)
		}

		fmt.Fprintf(os.Stderr, "wrote %s, signed by the new Talos CA\n", render.Rel(tc))
	}

	printRotationNext(cfg, old, true)

	return nil
}

func printRotationNext(cfg *config.Config, old []byte, kubernetes bool) {
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
}
