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
itself what the rendered config would change. Nothing is modified.

Like apply, each node is asked which API it answers first, so a node still in
maintenance mode can be diffed alongside the rest of the cluster.`

	return cmd
}

func applyLikeCmd(use, short string, forceDryRun bool) *cobra.Command {
	// What the remainder message says a stopped pass did not do.
	done := "applied"
	if forceDryRun {
		done = "diffed"
	}

	var (
		nodes      []string
		mode       string
		insecure   bool
		onlyNew    bool
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
		Long: `Apply renders each selected node's machine config and applies it, one node at a
time, waiting for the node to come back and the cluster to stay healthy before
moving to the next.

Each node is asked which API it answers before its config is sent: a node that
has joined is addressed with cluster PKI, and one in maintenance mode -- never
configured, or reset -- through the maintenance service, which is what adopting
it means. A mixed cluster therefore needs no flag. --only-new restricts a run
to the nodes in maintenance mode; -i forces the maintenance service for every
node, and --insecure=false forces cluster PKI.`,
		Args: cobra.NoArgs,
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

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)

			// An explicit --insecure (or --insecure=false) is an instruction,
			// not a hint: honour it for every node and skip the probing.
			forced := cmd.Flags().Changed("insecure")

			// What each node answered, so the health gate can tell a cluster
			// that is still being built from one that is misbehaving.
			modes := map[string]talosctl.Mode{}

			// The gate standing down is worth saying once, not after every
			// node: the reason does not change between them, and a roll-out
			// across a cluster being built would otherwise repeat it all the
			// way down the output.
			var saidUngated bool

			if onlyNew {
				targets, err = newNodes(tal, tc, targets)
				if err != nil {
					return err
				}

				if len(targets) == 0 {
					fmt.Fprintf(os.Stderr, "nothing to adopt: no selected node is in maintenance mode\n")

					return nil
				}
			}

			for i, n := range targets {
				file := cfg.MachineConfigPath(n)

				if _, err := os.Stat(file); err != nil {
					return fmt.Errorf("no rendered config for %s at %s: run `talman render` first",
						n.Hostname, file)
				}

				// A cluster is rarely all one thing: after a reset, or when a
				// machine is added, some nodes answer with cluster PKI and
				// some only on the maintenance service. Asking each node
				// which it is costs one call and is the difference between
				// adopting a node and failing the run.
				maintenance := insecure

				if !forced {
					state := tal.Mode(tc, n.IPAddress)
					modes[n.IPAddress] = state

					switch state {
					case talosctl.ModeMaintenance:
						maintenance = true
					case talosctl.ModeRunning:
						maintenance = false
					case talosctl.ModeUnreachable:
						return fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service\n%s",
							n.Hostname, n.IPAddress, resumeHint(use, done, targets[i:]))
					}
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

				if maintenance {
					args = append(args, "--insecure")
				}

				args = append(args, extraFlags...)

				switch {
				case maintenance && forced:
					fmt.Fprintf(os.Stderr, "== %s (%s) through the maintenance service (--insecure)\n",
						n.Hostname, n.IPAddress)
				case maintenance:
					fmt.Fprintf(os.Stderr, "== %s (%s) maintenance mode; adopting it\n", n.Hostname, n.IPAddress)
				default:
					fmt.Fprintf(os.Stderr, "== %s (%s)\n", n.Hostname, n.IPAddress)
				}

				if err := tal.Stream(args...); err != nil {
					return fmt.Errorf("%w\n%s", err, resumeHint(use, done, targets[i:]))
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

					// It answered on the secure API, so whatever it was
					// before this, it is in the cluster now.
					modes[n.IPAddress] = talosctl.ModeRunning
				}

				// The gate checks the cluster the config describes, so it
				// only means something once that cluster exists. A node that
				// has not been adopted yet answers with a self-signed
				// maintenance certificate, which the check reports as
				// "certificate signed by unknown authority" -- a build-out
				// step read as a broken cluster.
				//
				// So while any node is outside the cluster, the gate stands
				// down and says which nodes those are. Once they have all
				// joined it gates every node, which is the roll-out case it
				// exists for. An explicit --health always gates.
				if health && !cmd.Flags().Changed("health") {
					if outside := notInCluster(cfg, tal, tc, modes); len(outside) > 0 {
						if !saidUngated {
							saidUngated = true

							fmt.Fprintf(os.Stderr, "   not gating on health: %d node(s) not in the cluster yet "+
								"(--health to check anyway)\n", len(outside))

							// Which ones, for whoever is asking why.
							if opts.verbose {
								fmt.Fprintf(os.Stderr, "   %s\n", strings.Join(outside, ", "))
							}
						}

						continue
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
		"force the maintenance service for every node (default: ask each node which API it answers)")
	cmd.Flags().BoolVar(&onlyNew, "only-new", false,
		"restrict the run to nodes that are in maintenance mode")

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

// newNodes keeps the targets that are in maintenance mode, which is what
// "not adopted yet" looks like from outside.
//
// This is the one path that probes every target up front rather than node by
// node: the filter cannot be applied without knowing all the answers, and
// paying for them is the point of asking for it.
func newNodes(tal *talosctl.Runner, talosconfig string, targets []*config.Node) ([]*config.Node, error) {
	out := make([]*config.Node, 0, len(targets))

	for _, n := range targets {
		switch state := tal.Mode(talosconfig, n.IPAddress); state {
		case talosctl.ModeMaintenance:
			out = append(out, n)
		case talosctl.ModeRunning:
			fmt.Fprintf(os.Stderr, "   %s (%s) is running; not new, skipping\n", n.Hostname, n.IPAddress)
		case talosctl.ModeUnreachable:
			return nil, fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service: "+
				"--only-new cannot tell whether it needs adopting\n"+
				"  bring it up, or narrow the run with -n", n.Hostname, n.IPAddress)
		}
	}

	return out, nil
}

// notInCluster describes the configured nodes that are not part of the cluster
// yet, empty when every one of them is.
//
// Nodes this run has already asked about are not asked again: the answers are
// carried in modes, including for a node this run just adopted and watched
// come back.
func notInCluster(cfg *config.Config, tal *talosctl.Runner, talosconfig string,
	modes map[string]talosctl.Mode,
) []string {
	var outside []string

	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]

		state, known := modes[n.IPAddress]
		if !known {
			state = tal.Mode(talosconfig, n.IPAddress)
			modes[n.IPAddress] = state
		}

		if state != talosctl.ModeRunning {
			outside = append(outside, fmt.Sprintf("%s is %s", n.Hostname, state))
		}
	}

	return outside
}

// clusterHealth runs the same check `talman health` performs, from the first
// control plane in the config.
//
// Not from the node just applied: a worker cannot answer for the cluster, and
// picking whichever node the roll-out happens to be on would make a failing
// gate mean something different at each step.
func clusterHealth(cfg *config.Config, tal *talosctl.Runner, talosconfig string) error {
	from, err := healthNode(cfg, tal, talosconfig)
	if err != nil {
		return err
	}

	// Client-side: the gate is "can talman still see a healthy cluster from
	// here", which is the question a roll-out has to stop on.
	return tal.Stream(healthArgs(cfg, talosconfig, from, false)...)
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
