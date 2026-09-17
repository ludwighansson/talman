package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
)

func newResetCmd() *cobra.Command {
	var (
		nodes      []string
		yes        bool
		graceful   bool
		direct     bool
		reboot     bool
		wipeDisk   bool
		wipeLabels []string
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe nodes and return them to maintenance mode",
		Long: `Reset returns a node to maintenance mode. The EPHEMERAL and STATE partitions
are wiped -- all data, and the machine config with it -- and the node reboots
with Talos still installed, waiting for a config. Run against enough control
planes, it destroys the cluster.

That is talosctl's --system-labels-to-wipe, not its default: left to itself
talosctl wipes the disk whole, bootloader included, which leaves a machine with
nothing to boot rather than a node in maintenance mode. --wipe-disk asks for
that reinstall-me state deliberately.

Workers are reset before control planes, and each node is reached at its own
address rather than through the talosconfig endpoints. Both exist for the same
reason: the endpoints are the control planes, so wiping those first destroys
the path to every node still waiting -- and a graceful reset needs a live
cluster to leave.`,
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

			if wipeDisk && cmd.Flags().Changed("wipe-labels") {
				return fmt.Errorf("--wipe-disk and --wipe-labels ask for different resets: " +
					"--wipe-disk wipes the system disk whole, --wipe-labels wipes the partitions " +
					"it names and leaves the rest of the disk alone")
			}

			// An empty label list is talosctl's whole-disk wipe. Reaching the
			// most destructive behaviour by emptying a list, rather than by
			// asking for it, is not something a reset should allow.
			if !wipeDisk && len(wipeLabels) == 0 {
				return fmt.Errorf("--wipe-labels is empty, which wipes the whole disk: " +
					"pass --wipe-disk if that is what you mean")
			}

			// Before the confirmation, so the order shown is the order run.
			targets = resetOrder(targets)

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			if !yes {
				if err := confirm(cfg.ClusterName, targets, consequence(wipeDisk, wipeLabels, reboot)); err != nil {
					return err
				}
			}

			tal := runner(cfg)

			for i, n := range targets {
				args := []string{"--talosconfig", tc}

				// Ahead of the subcommand: --endpoints is one of talosctl's
				// global flags, and it overrides the talosconfig's list for
				// this call only.
				if direct {
					args = append(args, "--endpoints", n.IPAddress)
				}

				args = append(args,
					"reset",
					"--nodes", n.IPAddress,
					fmt.Sprintf("--graceful=%t", graceful),
					fmt.Sprintf("--reboot=%t", reboot),
				)

				// Omitted entirely for a whole-disk wipe: that is what
				// talosctl does when no labels are named.
				if !wipeDisk {
					args = append(args, "--system-labels-to-wipe", strings.Join(wipeLabels, ","))
				}

				args = append(args, extraFlags...)

				fmt.Fprintf(os.Stderr, "== resetting %s (%s)\n", n.Hostname, n.IPAddress)

				if err := tal.Stream(args...); err != nil {
					var flags []string
					if !graceful {
						flags = append(flags, "--graceful=false")
					}

					return fmt.Errorf("%w\n%s", err, resumeHint("reset", "reset", targets[i:], flags...))
				}
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&graceful, "graceful", true, "leave etcd cleanly before resetting")
	cmd.Flags().BoolVar(&direct, "direct", true,
		"reach each node at its own address instead of proxying through the talosconfig endpoints")
	cmd.Flags().BoolVar(&reboot, "reboot", true,
		"reboot the node after resetting, rather than shutting it down")
	cmd.Flags().StringSliceVar(&wipeLabels, "wipe-labels", []string{"EPHEMERAL", "STATE"},
		"system partitions to wipe, by label")
	cmd.Flags().BoolVar(&wipeDisk, "wipe-disk", false,
		"wipe the system disk whole, Talos installation included, instead of named partitions")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// resetOrder puts workers before control planes.
//
// talman reaches a node through the talosconfig endpoints, and those are the
// control planes. Config order lists control planes first, so a whole-cluster
// reset used to cut its own path partway through: three control planes wiped,
// then the first worker's reset proxied through one of them and timed out.
//
// It is the right order for the cluster too. A graceful reset asks the node to
// leave etcd and the Kubernetes API, which needs a control plane still
// serving; taking the control planes out first turns every remaining graceful
// reset into a failure.
//
// Stable within each group, so nodes still go in config order.
func resetOrder(targets []*config.Node) []*config.Node {
	out := make([]*config.Node, 0, len(targets))

	for _, n := range targets {
		if !n.IsControlPlane() {
			out = append(out, n)
		}
	}

	for _, n := range targets {
		if n.IsControlPlane() {
			out = append(out, n)
		}
	}

	return out
}

// consequence describes what this particular reset will leave behind.
//
// The prompt is where an operator decides, so it has to name the outcome the
// flags actually produce: "destroys all data" reads the same whether the node
// comes back in maintenance mode or stops booting altogether.
func consequence(wipeDisk bool, wipeLabels []string, reboot bool) string {
	var what, then string

	// STATE holds the machine config, so it is what decides whether the node
	// comes back asking for one or comes back as itself.
	state := wipeDisk

	for _, l := range wipeLabels {
		if strings.EqualFold(l, "STATE") {
			state = true
		}
	}

	if wipeDisk {
		what = "wipes their system disks whole, Talos installation included"
	} else {
		what = "wipes " + strings.Join(wipeLabels, " and ") + ", destroying all data on them"

		if state {
			what += " and their machine configs"
		}
	}

	switch {
	case !reboot:
		then = "shuts them down"
	case wipeDisk:
		then = "reboots them with nothing left to boot"
	case state:
		then = "reboots them into maintenance mode"
	default:
		then = "reboots them, still holding their machine configs"
	}

	return "This " + what + ", then " + then + "."
}

// confirm requires the operator to type the cluster name, so a reset cannot
// happen because a script passed the wrong config file.
func confirm(clusterName string, targets []*config.Node, consequence string) error {
	fmt.Fprintf(os.Stderr, "About to reset %d node(s) in cluster %q:\n", len(targets), clusterName)

	for _, n := range targets {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n", n.Hostname, n.IPAddress)
	}

	fmt.Fprintf(os.Stderr, "%s Type the cluster name to continue: ", consequence)

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reset aborted: %w", err)
	}

	if strings.TrimSpace(line) != clusterName {
		return fmt.Errorf("reset aborted: input did not match %q", clusterName)
	}

	return nil
}
