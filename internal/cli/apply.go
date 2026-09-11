package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// applyModes are the apply modes talosctl v1.14 accepts. "reboot" was removed
// in 1.14 and is rejected here with a pointer at the replacement, rather than
// being passed through to a confusing talosctl error.
var applyModes = []string{"auto", "no-reboot", "staged", "try"}

func newApplyCmd() *cobra.Command {
	return applyLikeCmd("apply", "Apply rendered machine configs to the cluster", false)
}

func newDiffCmd() *cobra.Command {
	cmd := applyLikeCmd("diff", "Show what applying the rendered configs would change", true)
	cmd.Long = `Diff runs "talosctl apply-config --dry-run" for each node, which asks the node
itself what the rendered config would change. Nothing is modified.`

	return cmd
}

func applyLikeCmd(use, short string, forceDryRun bool) *cobra.Command {
	var (
		nodes      []string
		mode       string
		insecure   bool
		dryRun     bool
		wait       bool
		health     bool
		noRender   bool
		extraFlags []string
		stabilize  time.Duration
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			if forceDryRun {
				dryRun = true
			}

			if mode == "reboot" {
				return fmt.Errorf("--mode=reboot was removed in Talos 1.14; " +
					"use --mode=auto (apply now, reboot only if required) or --mode=staged")
			}

			if !slices.Contains(applyModes, mode) {
				return fmt.Errorf("--mode %q is invalid: must be one of %s",
					mode, strings.Join(applyModes, ", "))
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			// Render first, every time.
			//
			// Applying whatever happens to be sitting in the output directory
			// means a patch added since the last render is silently not
			// applied -- the change looks like it landed and did not. It also
			// makes `diff` compare live state against a stale artefact and
			// report agreement. Rendering here is what makes both commands
			// mean what they say.
			if !noRender {
				if err := renderForApply(cfg, targets); err != nil {
					return err
				}
			}

			tc, err := requireTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)

			for _, n := range targets {
				file := cfg.MachineConfigPath(n)

				if _, err := os.Stat(file); err != nil {
					return fmt.Errorf("no rendered config for %s at %s: run `talman render` first",
						n.Hostname, file)
				}

				args := []string{
					"--talosconfig", tc,
					"apply-config",
					"--nodes", n.IPAddress,
					"--file", file,
					"--mode", mode,
				}

				if dryRun {
					args = append(args, "--dry-run")
				}

				if insecure {
					args = append(args, "--insecure")
				}

				args = append(args, extraFlags...)

				fmt.Fprintf(os.Stderr, "== %s (%s)\n", n.Hostname, n.IPAddress)

				if err := tal.Stream(args...); err != nil {
					return err
				}

				// Nothing to wait for when the change was not enacted: a dry
				// run touches nothing, and staged config lands on the next
				// reboot rather than now.
				if dryRun || mode == "staged" {
					continue
				}

				if wait {
					logf := func(format string, args ...any) {
						fmt.Fprintf(os.Stderr, format+"\n", args...)
					}

					if err := tal.WaitReady(tc, n.IPAddress, stabilize, timeout, logf); err != nil {
						return fmt.Errorf("%w\n  the remaining nodes were left untouched; "+
							"re-run once it recovers, or pass --wait=false to roll on regardless", err)
					}
				}

				if health {
					fmt.Fprintf(os.Stderr, "   checking cluster health before continuing\n")

					if err := clusterHealth(cfg, tal, tc); err != nil {
						return fmt.Errorf("cluster is unhealthy after applying to %s: %w\n"+
							"  the remaining nodes were left untouched", n.Hostname, err)
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().StringVarP(&mode, "mode", "m", "auto",
		"apply mode: "+strings.Join(applyModes, ", "))
	cmd.Flags().BoolVarP(&insecure, "insecure", "i", false,
		"use the maintenance service (for nodes that have not joined yet)")

	if !forceDryRun {
		cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would change without applying")
	}

	// Rolling straight on to the next node is how a bad config takes out a
	// whole control plane instead of one machine, so both gates default on.
	cmd.Flags().BoolVar(&wait, "wait", true,
		"wait for each node to come back before applying to the next")
	cmd.Flags().BoolVar(&health, "health", true,
		"run a cluster health check between nodes")
	cmd.Flags().DurationVar(&stabilize, "stabilize", 30*time.Second,
		"how long a node must stay reachable before it counts as back")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute,
		"how long to wait for a single node to come back")
	cmd.Flags().BoolVar(&noRender, "no-render", false,
		"apply the configs already in the output directory instead of re-rendering")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// clusterHealth runs the same check `talman health` performs.
func clusterHealth(cfg *config.Config, tal *talosctl.Runner, talosconfig string) error {
	var cps, workers []string

	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if n.IsControlPlane() {
			cps = append(cps, n.IPAddress)
		} else {
			workers = append(workers, n.IPAddress)
		}
	}

	args := []string{"--talosconfig", talosconfig, "health", "--server=false"}

	if len(cps) > 0 {
		args = append(args, "--control-plane-nodes", strings.Join(cps, ","))
	}

	if len(workers) > 0 {
		args = append(args, "--worker-nodes", strings.Join(workers, ","))
	}

	return tal.Stream(args...)
}

// renderForApply re-renders the targeted nodes and writes them out, so what
// gets applied is what the config and patches currently say.
//
// The talosconfig is regenerated too: it is the credential the apply itself
// uses, and leaving it behind while the machine configs move forward is how a
// cluster ends up unreachable by its own tooling.
func renderForApply(cfg *config.Config, targets []*config.Node) error {
	r, err := newRenderer(cfg, false, os.Stderr)
	if err != nil {
		return err
	}
	defer r.Close()

	results := make([]*render.Result, 0, len(targets))

	for _, n := range targets {
		res, err := r.Node(n)
		if err != nil {
			return err
		}

		if err := r.Validate(res, cfg.TalosMode); err != nil {
			return err
		}

		results = append(results, res)
	}

	return r.WriteAll(results, true)
}
