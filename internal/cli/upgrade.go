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
		health     bool
		timeout    time.Duration
		snapshot   bool
		waves      waveFlags
		parallel   int
		detailed   bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade Talos on nodes to their configured installer image",
		Long: `Upgrade moves each node to the installer image its schematic and talosVersion
resolve to, skipping nodes that already run it. Nodes go one at a time, in
config order or rollout waves; control planes always alone, --parallel batches
workers. --dry-run names the nodes an upgrade would reach, --health gates
between nodes, --snapshot takes an etcd snapshot first.

--detailed-exit-code: 2 if a node was upgraded, 0 if none needed it, 1 on error.

More in the README: "Rolling changes out safely", "Rolling out in waves".`,
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

			targets, err := selectNodes(cfg, nodes)
			if err != nil {
				return err
			}

			stages, targets, thenFrom, err := waves.plan(cfg, targets)
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
				skipped  int
				upgraded int
				countMu  sync.Mutex

				// The snapshot waits for the first node that is actually
				// going to be upgraded: a run that finds nothing to do has
				// no reason to stream every Secret in the cluster to disk.
				snapOnce sync.Once
				snapErr  error
			)

			upgradeOne := func(n *config.Node, grouped bool, say func(string)) (bool, error) {
				ctx, err := r.Context(n)
				if err != nil {
					return false, err
				}

				want := ctx.Node.TalosVersion
				header := fmt.Sprintf("== %s (%s)\n", n.Hostname, n.IPAddress)

				rec.NodeInfo(n.Hostname, "to_version", want)

				if !force {
					current, err := tal.State(tc, n.IPAddress)
					if err != nil {
						return false, fmt.Errorf("reading current state of %s: %w "+
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

						return false, nil
					}

					header = fmt.Sprintf("== %s (%s): %s -> %s\n",
						n.Hostname, n.IPAddress, describeState(current), want)
				}

				header += fmt.Sprintf("   image %s\n", ctx.Node.InstallerImage)

				// Counted once it has happened, not when it is attempted: a
				// node whose upgrade failed did not change as far as anyone
				// can tell. A dry run counts what would change, as apply's
				// does, and its metrics carry dry_run="true" to say so.
				counted := func() {
					countMu.Lock()
					upgraded++
					countMu.Unlock()

					rec.NodeChanged(n.Hostname, true)
				}

				if dryRun {
					say(header + "   would upgrade (dry run)\n")
					counted()

					return false, nil
				}

				if snapshot {
					snapOnce.Do(func() { snapErr = etcdSnapshot(cfg, tal, tc, nil, "") })

					if snapErr != nil {
						return false, fmt.Errorf("taking the etcd snapshot --snapshot asked for: %w", snapErr)
					}
				}

				args := append([]string{
					"--talosconfig", tc,
					"upgrade",
					"--nodes", n.IPAddress,
					"--image", ctx.Node.InstallerImage,
					"--timeout", timeout.String(),
				}, extraFlags...)

				if err := runTalosctl(tal, grouped, header, say, args); err != nil {
					return false, err
				}

				counted()

				return true, nil
			}

			// Control planes one at a time whatever --parallel says: an
			// upgrade reboots the machine, and two control planes rebooting
			// together is how a three-node cluster loses quorum.
			if err := (rollOut{
				cmd: cmd, cfg: cfg, tal: tal, tc: tc,
				verb: "upgrade", done: "upgraded",
				targets: targets, parallel: parallel, inert: dryRun,
				// talosctl waits for every upgrade: it drains the node first,
				// and a drain always waits.
				health: health, timeout: timeout, waits: true,
				stages: stages, soak: cfg.Rollout.SoakDuration(), thenFrom: thenFrom,
			}).run(upgradeOne); err != nil {
				return err
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
	cmd.Flags().BoolVar(&health, "health", false,
		"run a cluster health check between nodes, and stop if it fails")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute,
		"how long to wait for each node's upgrade, and for the health check between nodes")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false,
		"save an etcd snapshot to the output directory before upgrading anything")
	addWaveFlags(cmd, &waves)
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
				if err := etcdSnapshot(cfg, tal, tc, nil, ""); err != nil {
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
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"say whether an upgrade is needed, without running one (with --force: talosctl's own plan)")
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
