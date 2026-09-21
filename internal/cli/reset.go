package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

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
		parallel   int
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
				return errors.New("--wipe-disk and --wipe-labels ask for different resets: " +
					"--wipe-disk wipes the system disk whole, --wipe-labels wipes the partitions " +
					"it names and leaves the rest of the disk alone")
			}

			// An empty label list is talosctl's whole-disk wipe. Reaching the
			// most destructive behaviour by emptying a list, rather than by
			// asking for it, is not something a reset should allow.
			if !wipeDisk && len(wipeLabels) == 0 {
				return errors.New("--wipe-labels is empty, which wipes the whole disk: " +
					"pass --wipe-disk if that is what you mean")
			}

			// Before the confirmation, so the order shown is the order run.
			targets = resetOrder(targets)

			// A graceful reset asks etcd to remove the node from its member
			// list, and etcd will only agree while enough members are left to
			// agree on anything. Resetting every control plane in the config
			// is destroying the cluster, so the last one asks a cluster that
			// can no longer answer:
			//
			//   failed to leave cluster: failed to remove member ...:
			//   etcdserver: re-configuration failed due to not enough
			//   started members
			//
			// There is nothing to leave cleanly when nothing is left, so the
			// control planes skip the attempt. Workers still leave gracefully:
			// they go first, while the cluster is still serving.
			destroying := graceful && allControlPlanes(cfg, targets)

			if destroying {
				if cmd.Flags().Changed("graceful") {
					fmt.Fprintf(os.Stderr, "warning: --graceful with every control plane selected destroys the "+
						"cluster, so the last one will have no etcd left to leave and will fail there\n")

					destroying = false
				} else {
					fmt.Fprintf(os.Stderr, "every control plane is selected, so the cluster is being destroyed: "+
						"control planes will not try to leave etcd first\n")
				}
			}

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

			var printMu sync.Mutex

			say := func(block string) {
				printMu.Lock()
				defer printMu.Unlock()

				fmt.Fprint(os.Stderr, block)
			}

			resetOne := func(n *config.Node, grouped bool) error {
				args := []string{"--talosconfig", tc}

				// Ahead of the subcommand: --endpoints is one of talosctl's
				// global flags, and it overrides the talosconfig's list for
				// this call only.
				if direct {
					args = append(args, "--endpoints", n.IPAddress)
				}

				nodeGraceful := graceful && (!destroying || !n.IsControlPlane())

				args = append(args,
					"reset",
					"--nodes", n.IPAddress,
					fmt.Sprintf("--graceful=%t", nodeGraceful),
					fmt.Sprintf("--reboot=%t", reboot),
				)

				// Omitted entirely for a whole-disk wipe: that is what
				// talosctl does when no labels are named.
				if !wipeDisk {
					args = append(args, "--system-labels-to-wipe", strings.Join(wipeLabels, ","))
				}

				args = append(args, extraFlags...)

				header := fmt.Sprintf("== resetting %s (%s)\n", n.Hostname, n.IPAddress)

				if grouped {
					out, err := tal.Combined(args...)
					if err != nil {
						say(header + string(out) + "   error: " + err.Error() + "\n")
					} else {
						say(header + string(out))
					}

					return err
				}

				say(header)

				return tal.Stream(args...)
			}

			// Control planes one at a time whatever --parallel says. A batch
			// of workers can be wiped together; two control planes cannot,
			// and the ordering above puts them last for the same reason.
			done := 0

			var (
				okMu      sync.Mutex
				succeeded = map[string]bool{}
			)

			for _, batch := range batches(targets, parallel) {
				grouped := len(batch) > 1

				if _, err := eachNode(batch, len(batch), func(n *config.Node) (struct{}, error) {
					if err := resetOne(n, grouped); err != nil {
						return struct{}{}, err
					}

					okMu.Lock()
					succeeded[n.IPAddress] = true
					okMu.Unlock()

					return struct{}{}, nil
				}); err != nil {
					var flags []string
					if !graceful {
						flags = append(flags, "--graceful=false")
					}

					return fmt.Errorf("%w\n%s", err,
						resumeHint("reset", "reset", without(targets[done:], succeeded), flags...))
				}

				done += len(batch)
			}

			fmt.Fprintf(os.Stderr, "%d node(s) reset successfully\n", len(targets))

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
	addParallelFlag(cmd, &parallel, 1,
		"how many workers to wipe at once; control planes always go one at a time")
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

// allControlPlanes reports whether a selection takes out every control plane
// the config knows about, which is the same question as whether the cluster
// survives the run.
func allControlPlanes(cfg *config.Config, targets []*config.Node) bool {
	cps := cfg.ControlPlanes()
	if len(cps) == 0 {
		return false
	}

	selected := make(map[string]bool, len(targets))
	for _, n := range targets {
		selected[n.IPAddress] = true
	}

	for _, cp := range cps {
		if !selected[cp.IPAddress] {
			return false
		}
	}

	return true
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
