package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/factory"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newUpgradeCmd() *cobra.Command {
	var (
		nodes         []string
		submit        bool
		stage         bool
		force         bool
		skipEtcdCheck bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Talos on nodes to their configured installer image",
		Long: `Upgrade runs "talosctl upgrade" against each node with the installer image that
node's schematic and talosVersion resolve to -- the same reference talman
passes to gen config, so an upgrade cannot drift from what render produced.

Nodes are upgraded one at a time in config order. Restrict the set with --node
and check the target first with "talman image url".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			tc, err := requireTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)
			r := &render.Renderer{Cfg: cfg, Submit: submit}

			var skipped int

			for _, n := range targets {
				ctx, err := r.Context(n)
				if err != nil {
					return err
				}

				want := ctx.Node.TalosVersion

				if !force {
					current, err := tal.State(tc, n.IPAddress)
					if err != nil {
						return fmt.Errorf("reading current state of %s: %w "+
							"(pass --force to upgrade without checking)", n.Hostname, err)
					}

					if upToDate(current, want, ctx.Node.SchematicID) {
						fmt.Fprintf(os.Stderr, "== %s (%s) already runs %s with schematic %s; skipping\n",
							n.Hostname, n.IPAddress, current.TalosVersion, short(current.SchematicID))

						skipped++

						continue
					}

					fmt.Fprintf(os.Stderr, "== %s (%s): %s -> %s\n",
						n.Hostname, n.IPAddress, describeState(current), want)
				}

				args := []string{
					"--talosconfig", tc,
					"upgrade",
					"--nodes", n.IPAddress,
					"--image", ctx.Node.InstallerImage,
				}

				if stage {
					args = append(args, "--stage")
				}

				if skipEtcdCheck {
					args = append(args, "--force")
				}

				fmt.Fprintf(os.Stderr, "   image %s\n", ctx.Node.InstallerImage)

				if err := tal.Stream(args...); err != nil {
					return err
				}
			}

			if skipped == len(targets) {
				fmt.Fprintf(os.Stderr, "nothing to upgrade: every selected node already runs "+
					"its configured version and schematic (use --force to upgrade anyway)\n")
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&submit, "submit", false, "register schematics with the Image Factory first")
	cmd.Flags().BoolVar(&stage, "stage", false, "stage the upgrade to apply on next reboot")
	cmd.Flags().BoolVar(&force, "force", false,
		"upgrade even when the node already runs the configured version and schematic")
	cmd.Flags().BoolVar(&skipEtcdCheck, "skip-etcd-check", false,
		"pass --force to talosctl, skipping its etcd health checks")

	return cmd
}

func newUpgradeK8sCmd() *cobra.Command {
	var (
		node   string
		to     string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade-k8s",
		Short: "Upgrade Kubernetes to the configured version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			version := to
			if version == "" {
				version = strings.TrimPrefix(cfg.KubernetesVersion, "v")
			}

			args := []string{
				"--talosconfig", tc,
				"upgrade-k8s",
				"--nodes", target.IPAddress,
				"--to", version,
			}

			if dryRun {
				args = append(args, "--dry-run")
			}

			return tal.Stream(args...)
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to drive the upgrade from")
	cmd.Flags().StringVar(&to, "to", "", "target Kubernetes version (default: kubernetesVersion from the config)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the upgrade plan without running it")

	return cmd
}

// upToDate reports whether a node already runs the configured image.
//
// The comparison is against the node's *running* state, so it deliberately
// ignores the registry and repository half of the installer reference: those
// decide where the next image is pulled from, not what is running, and
// changing a mirror is not a reason to reboot a cluster. --force covers the
// case where you want the upgrade regardless.
//
// Anything talman could not determine counts as not up to date: an unknown
// version or schematic must not be read as agreement.
func upToDate(current talosctl.NodeState, wantVersion, wantSchematic string) bool {
	if current.TalosVersion == "" || current.TalosVersion != wantVersion {
		return false
	}

	// A node installed from something other than a factory image reports no
	// schematic. Without one there is nothing to compare, so the version
	// match alone decides.
	if current.SchematicID == "" {
		return wantSchematic == "" || wantSchematic == factory.VanillaID
	}

	return current.SchematicID == wantSchematic
}

func describeState(s talosctl.NodeState) string {
	switch {
	case s.TalosVersion == "":
		return "unknown version"
	case s.SchematicID == "":
		return s.TalosVersion
	default:
		return s.TalosVersion + " (schematic " + short(s.SchematicID) + ")"
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}
