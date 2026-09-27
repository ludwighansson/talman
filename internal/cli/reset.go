package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
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
		Long: `Reset wipes the EPHEMERAL and STATE partitions and reboots each node into
maintenance mode, Talos still installed. It asks for the cluster name first,
unless --yes. Workers go before control planes, each reached at its own
address. --wipe-labels narrows what is wiped, --wipe-disk wipes the whole
disk, --reboot=false shuts down instead.

More in the README: "Rolling changes out safely".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec := currentRun

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := selectNodes(cfg, nodes)
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

			for _, n := range targets {
				rec.Plan(n.Hostname, string(n.Role))
			}

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

			resetOne := func(n *config.Node, grouped bool, say func(string)) (bool, error) {
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

				err := runTalosctl(tal, grouped, header, say, args)
				if err == nil {
					rec.NodeChanged(n.Hostname, true)
				}

				return true, err
			}

			// Control planes one at a time whatever --parallel says, workers
			// in batches; the ordering above puts control planes last. Each
			// reset returns only once its node is done, so there is nothing
			// for the roll-out to wait for.
			ro := rollOut{
				cmd: cmd, cfg: cfg, tal: tal, tc: tc,
				verb: "reset", done: "reset",
				targets: targets, parallel: parallel, waits: true,
				// --yes is left off: the resumed reset wipes a different set
				// of machines, and should ask about them.
				hintDrop: []string{"yes"},
				hintAdd: func(left []*config.Node) []string {
					// This run skipped leaving etcd because it was destroying
					// the cluster. Once only some control planes are left
					// they are no longer every control plane, so the resumed
					// run has to be told, or it would try to leave a cluster
					// too small to let it. While workers remain, every control
					// plane does too, and the resumed run sees the teardown
					// for itself.
					if destroying && !cmd.Flags().Changed("graceful") && onlyControlPlanes(left) {
						return []string{"--graceful=false"}
					}

					return nil
				},
			}

			if err := ro.run(resetOne); err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "%d node(s) reset successfully\n", len(targets))

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
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
// control planes: resetting them first would cut the path to every worker
// after them.
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

	return typeClusterName("reset", clusterName, consequence)
}

// typeClusterName asks for the cluster name and fails unless it is typed back.
// what names the operation in the refusal.
func typeClusterName(what, clusterName, consequence string) error {
	fmt.Fprintf(os.Stderr, "%s Type the cluster name to continue: ", consequence)

	// Read off to the side: the signal handler keeps Ctrl-C from ending the
	// process, so a read blocked on the terminal would otherwise be the one
	// place an interrupt could not get out of.
	type answer struct {
		line string
		err  error
	}

	answered := make(chan answer, 1)

	go func() {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		answered <- answer{line, err}
	}()

	var line string

	select {
	case a := <-answered:
		if a.err != nil {
			return fmt.Errorf("%s aborted: %w", what, a.err)
		}

		line = a.line
	case <-interrupt.Context().Done():
		return fmt.Errorf("%s aborted: interrupted", what)
	}

	if strings.TrimSpace(line) != clusterName {
		return fmt.Errorf("%s aborted: input did not match %q", what, clusterName)
	}

	return nil
}

func onlyControlPlanes(nodes []*config.Node) bool {
	for _, n := range nodes {
		if !n.IsControlPlane() {
			return false
		}
	}

	return len(nodes) > 0
}
