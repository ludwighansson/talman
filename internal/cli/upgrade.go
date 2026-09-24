package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/factory"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newUpgradeCmd() *cobra.Command {
	var (
		nodes      []string
		submit     bool
		force      bool
		dryRun     bool
		wait       bool
		health     bool
		timeout    time.Duration
		snapshot   bool
		parallel   int
		detailed   bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Talos on nodes to their configured installer image",
		Long: `Upgrade runs "talosctl upgrade" against each node with the installer image that
node's schematic and talosVersion resolve to -- the same reference talman
passes to gen config, so an upgrade cannot drift from what render produced.

Nodes are upgraded one at a time in config order, and talosctl waits for each
to come back on its new version before the next begins. Restrict the set with
--node, and see what would happen first with --dry-run: it asks every node what
it runs and names the ones an upgrade would reach, without upgrading any.

--health adds a cluster health check between nodes, and stops the roll-out if
the cluster is unhealthy. As with apply, it is off by default.

--detailed-exit-code reports whether anything was upgraded: 2 when at least one
node was, 0 when every selected node already ran its configured version and
schematic, 1 on error.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec := currentRun

			if dryRun {
				rec.SetLabel("dry_run", "true")

				if force {
					return errors.New("--dry-run and --force cannot be combined: " +
						"--force skips the question --dry-run answers")
				}
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			for _, n := range targets {
				rec.Plan(n.Hostname, string(n.Role))
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)
			r := &render.Renderer{Cfg: cfg, Submit: submit}

			var (
				skipped    int
				upgraded   int
				countMu    sync.Mutex
				upgradeOne func(*config.Node, bool, func(string)) error
			)

			upgradeOne = func(n *config.Node, grouped bool, say func(string)) error {
				ctx, err := r.Context(n)
				if err != nil {
					return err
				}

				want := ctx.Node.TalosVersion
				header := fmt.Sprintf("== %s (%s)\n", n.Hostname, n.IPAddress)

				rec.NodeInfo(n.Hostname, "to_version", want)

				if !force {
					current, err := tal.State(tc, n.IPAddress)
					if err != nil {
						return fmt.Errorf("reading current state of %s: %w "+
							"(pass --force to upgrade without checking)", n.Hostname, err)
					}

					rec.NodeInfo(n.Hostname, "from_version", current.TalosVersion)

					if upToDate(current, want, ctx.Node.SchematicID) {
						rec.NodeChanged(n.Hostname, false)
						say(fmt.Sprintf("== %s (%s) already runs %s with schematic %s; skipping\n",
							n.Hostname, n.IPAddress, current.TalosVersion, short(current.SchematicID)))

						countMu.Lock()
						skipped++
						countMu.Unlock()

						return nil
					}

					header = fmt.Sprintf("== %s (%s): %s -> %s\n",
						n.Hostname, n.IPAddress, describeState(current), want)
				}

				args := append([]string{
					"--talosconfig", tc,
					"upgrade",
					"--nodes", n.IPAddress,
					"--image", ctx.Node.InstallerImage,
					fmt.Sprintf("--wait=%t", wait),
					"--timeout", timeout.String(),
				}, extraFlags...)

				header += fmt.Sprintf("   image %s\n", ctx.Node.InstallerImage)

				// Counted once it has happened, not when it is attempted: a
				// node whose upgrade failed did not change as far as anyone
				// can tell, and the metrics must not say it did.
				counted := func() {
					countMu.Lock()
					upgraded++
					countMu.Unlock()

					rec.NodeChanged(n.Hostname, true)
				}

				if dryRun {
					say(header + "   would upgrade (dry run)\n")
					counted()

					return nil
				}

				if grouped {
					out, err := tal.Combined(args...)
					if err != nil {
						say(header + string(out) + "   error: " + err.Error() + "\n")

						return err
					}

					say(header + string(out))
					counted()

					return nil
				}

				say(header)

				if err := tal.Stream(args...); err != nil {
					return err
				}

				counted()

				return nil
			}

			// Control planes one at a time whatever --parallel says: an
			// upgrade reboots the machine, and two control planes rebooting
			// together is how a three-node cluster loses quorum.
			done := 0

			var (
				okMu      sync.Mutex
				succeeded = map[string]bool{}
			)

			if snapshot && !dryRun {
				if _, err := etcdSnapshot(cfg, tal, tc, nil, ""); err != nil {
					return fmt.Errorf("taking the etcd snapshot --snapshot asked for: %w", err)
				}
			}

			// A dry run reboots nothing, so there is nothing to keep apart.
			inert := dryRun
			if inert && !cmd.Flags().Changed("parallel") {
				parallel = len(targets)
			}

			for _, batch := range batchesFor(targets, parallel, inert) {
				grouped := len(batch) > 1
				out := newInOrder(os.Stderr, batch)

				if _, err := eachNode(batch, len(batch), func(n *config.Node) (struct{}, error) {
					defer out.finish(n)

					rec.NodeStart(n.Hostname, string(n.Role))

					err := upgradeOne(n, grouped, func(s string) { out.say(n, s) })
					rec.NodeDone(n.Hostname, err)

					if err != nil {
						return struct{}{}, err
					}

					okMu.Lock()
					succeeded[n.IPAddress] = true
					okMu.Unlock()

					return struct{}{}, nil
				}); err != nil {
					return fmt.Errorf("%w\n%s", err,
						resumeHint("upgrade", "upgraded", without(targets[done:], succeeded),
							replayFlags(cmd, "node")...))
				}

				done += len(batch)

				// Between batches, never after the last: by then there is
				// nothing left for the gate to protect.
				if health && !dryRun && done < len(targets) {
					fmt.Fprintf(os.Stderr, "   checking cluster health before continuing\n")

					if err := clusterHealth(cfg, tal, tc, timeout); err != nil {
						return fmt.Errorf("cluster is unhealthy after upgrading %s: %w\n%s",
							names(batch), err, resumeHint("upgrade", "upgraded", targets[done:],
								replayFlags(cmd, "node")...))
					}
				}
			}

			if skipped == len(targets) {
				fmt.Fprintf(os.Stderr, "nothing to upgrade: every selected node already runs "+
					"its configured version and schematic (use --force to upgrade anyway)\n")
			}

			if detailed && upgraded > 0 {
				return errChanged
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&submit, "submit", false, "register schematics with the Image Factory first")
	cmd.Flags().BoolVar(&force, "force", false,
		"upgrade even when the node already runs the configured version and schematic")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"say which nodes would be upgraded, without upgrading any")
	cmd.Flags().BoolVar(&wait, "wait", true,
		"wait for each node to come back on its new version before moving on")
	cmd.Flags().BoolVar(&health, "health", false,
		"run a cluster health check between nodes, and stop if it fails")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute,
		"how long to wait for each node's upgrade, and for the health check between nodes")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false,
		"save an etcd snapshot to the output directory before upgrading anything")
	addParallelFlag(cmd, &parallel, 1,
		"how many workers to upgrade at once; control planes always go one at a time")
	addDetailedExitCode(cmd, &detailed)
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

func newUpgradeK8sCmd() *cobra.Command {
	var (
		node     string
		to       string
		dryRun   bool
		force    bool
		snapshot bool
		detailed bool
		extraK8s []string
	)

	cmd := &cobra.Command{
		Use:   "upgrade-k8s",
		Short: "Upgrade Kubernetes to the configured version",
		Long: `Upgrade-k8s runs "talosctl upgrade-k8s" from one control plane node, which
upgrades the whole cluster to kubernetesVersion from the config.

talman first asks every node in the config which Kubernetes version it runs,
and does nothing when they are all already on the target. --dry-run answers for
talman rather than for talosctl, so on an up-to-date cluster it reports nothing
to upgrade -- which is what running the command would do. Pass --force to run
the upgrade regardless, or --force --dry-run for talosctl's own plan.

--detailed-exit-code reports which of the two happened: 2 when the upgrade ran,
0 when every node was already on the target, 1 on error.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rec := currentRun

			if dryRun {
				rec.SetLabel("dry_run", "true")
			}

			cfg, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			version := to
			if version == "" {
				version = cfg.KubernetesVersion
			}

			// talosctl takes the version unprefixed; the config and the
			// kubelet image tag are both written either way.
			version = strings.TrimPrefix(version, "v")

			if !force {
				current, err := clusterRunsK8s(tal, tc, cfg.Nodes, version)
				if err != nil {
					return err
				}

				if current {
					fmt.Fprintf(os.Stderr, "nothing to upgrade: every node already runs Kubernetes v%s "+
						"(use --force to upgrade anyway)\n", version)
					rec.SetChanged(false)

					return nil
				}
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

			args = append(args, extraK8s...)

			if snapshot && !dryRun {
				if _, err := etcdSnapshot(cfg, tal, tc, nil, ""); err != nil {
					return fmt.Errorf("taking the etcd snapshot --snapshot asked for: %w", err)
				}
			}

			if err := tal.Stream(args...); err != nil {
				return err
			}

			rec.SetChanged(true)

			if detailed {
				return errChanged
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to drive the upgrade from")
	cmd.Flags().StringVar(&to, "to", "", "target Kubernetes version (default: kubernetesVersion from the config)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the upgrade plan without running it")
	cmd.Flags().BoolVar(&force, "force", false,
		"upgrade even when every node already runs the target version")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false,
		"save an etcd snapshot to the output directory before upgrading")
	addDetailedExitCode(cmd, &detailed)
	addExtraFlags(cmd, &extraK8s)

	return cmd
}

// clusterRunsK8s reports whether every node already runs the target
// Kubernetes version, printing what each node is on.
//
// Every node, not just the one driving the upgrade: upgrade-k8s is a
// cluster-wide operation, and a single worker left behind is the whole reason
// to run it again.
func clusterRunsK8s(tal *talosctl.Runner, talosconfig string, nodes []config.Node, want string) (bool, error) {
	done := true

	for i := range nodes {
		n := &nodes[i]

		current, err := tal.KubeletVersion(talosconfig, n.IPAddress)
		if err != nil {
			return false, fmt.Errorf("reading the Kubernetes version of %s: %w "+
				"(pass --force to upgrade without checking)", n.Hostname, err)
		}

		if k8sUpToDate(current, want) {
			fmt.Fprintf(os.Stderr, "== %s (%s) already runs Kubernetes %s\n",
				n.Hostname, n.IPAddress, current)

			continue
		}

		fmt.Fprintf(os.Stderr, "== %s (%s): Kubernetes %s -> v%s\n",
			n.Hostname, n.IPAddress, describeK8sVersion(current), want)

		done = false
	}

	return done, nil
}

// k8sUpToDate reports whether a node's kubelet already runs the target
// version.
//
// The kubelet is the signal because it is the one Kubernetes component every
// node runs, and the last one "talosctl upgrade-k8s" moves: the control plane
// components are upgraded before it, so a kubelet on the target version means
// the rest of the cluster got there first. An upgrade that failed part way
// through leaves the kubelet behind, which is what makes this check safe to
// skip on.
//
// As with the Talos check, anything talman could not determine counts as out
// of date: an unknown version must not be read as agreement.
func k8sUpToDate(current, want string) bool {
	current = strings.TrimPrefix(current, "v")
	want = strings.TrimPrefix(want, "v")

	return current != "" && current == want
}

func describeK8sVersion(current string) string {
	if current == "" {
		return "(unknown version)"
	}

	return current
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
