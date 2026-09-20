package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
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
	var (
		nodes      []string
		mode       string
		insecure   bool
		onlyNew    bool
		parallel   int
		detailed   bool
		dryRun     bool
		wait       bool
		health     bool
		noRender   bool
		extraFlags []string
		stabilize  time.Duration
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply rendered machine configs to the cluster",
		Long: `Apply renders each selected node's machine config and applies it, one node at a
time, waiting for the node to come back and the cluster to stay healthy before
moving to the next.

Each node is asked which API it answers before its config is sent: a node that
has joined is addressed with cluster PKI, and one in maintenance mode -- never
configured, or reset -- through the maintenance service, which is what adopting
it means. A mixed cluster therefore needs no flag. --only-new restricts a run
to the nodes in maintenance mode; -i forces the maintenance service for every
node, and --insecure=false forces cluster PKI.

--dry-run runs "talosctl apply-config --dry-run" instead, which asks each node
what the rendered config would change without changing it. Nothing is enacted,
so nothing is staggered: every node is asked at once and the waiting and
health checking are skipped.

--detailed-exit-code reports the answer as an exit code: 2 when a node changed
or would change, 0 when none did, 1 on error. It is the same question either
way, because an apply asks each node for its diff before sending the config.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
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
			// makes --dry-run compare live state against a stale artefact and
			// report agreement. Rendering here is what makes the command mean
			// what it says.
			if !noRender {
				if err := renderForApply(cfg, targets, parallel); err != nil {
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
				targets, err = newNodes(tal, tc, targets, parallel)
				if err != nil {
					return err
				}

				if len(targets) == 0 {
					fmt.Fprintf(os.Stderr, "nothing to adopt: no selected node is in maintenance mode\n")

					return nil
				}
			}

			// Everything the per-node pass touches is per node, except the
			// probe cache, which several nodes in a batch write at once.
			var modesMu sync.Mutex

			// Eight talosctl processes writing to one terminal produce an
			// interleaved mess nobody can attribute to a node, so a batch of
			// more than one captures each node's output and prints it whole.
			var (
				changedMu sync.Mutex
				changed   bool
			)

			// Whether the cluster has an etcd to join. Worked out at most
			// once, and only when a node adopted out of maintenance mode is
			// about to be waited for.
			var (
				bootstrapMu  sync.Mutex
				bootstrapped *bool
				adoptedEarly int
			)

			clusterBootstrapped := func() bool {
				bootstrapMu.Lock()
				defer bootstrapMu.Unlock()

				if bootstrapped == nil {
					up := false

					for _, cp := range cfg.ControlPlanes() {
						if tal.EtcdRunning(tc, cp.IPAddress) {
							up = true

							break
						}
					}

					bootstrapped = &up
				}

				return *bootstrapped
			}

			markChanged := func() {
				changedMu.Lock()
				defer changedMu.Unlock()

				changed = true
			}

			var printMu sync.Mutex

			say := func(block string) {
				printMu.Lock()
				defer printMu.Unlock()

				fmt.Fprint(os.Stderr, block)
			}

			applyOne := func(n *config.Node, grouped bool) error {
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

					modesMu.Lock()
					modes[n.IPAddress] = state
					modesMu.Unlock()

					switch state {
					case talosctl.ModeMaintenance:
						maintenance = true
					case talosctl.ModeRunning:
						maintenance = false
					case talosctl.ModeUnreachable:
						return fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service",
							n.Hostname, n.IPAddress)
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

				var header string

				switch {
				case maintenance && forced:
					header = fmt.Sprintf("== %s (%s) through the maintenance service (--insecure)\n",
						n.Hostname, n.IPAddress)
				case maintenance:
					header = fmt.Sprintf("== %s (%s) maintenance mode; adopting it\n", n.Hostname, n.IPAddress)
				default:
					header = fmt.Sprintf("== %s (%s)\n", n.Hostname, n.IPAddress)
				}

				// A real apply says nothing about whether the config it sent
				// differed from the one already there, so when the answer is
				// being reported as an exit code it is asked for first. The
				// dry run is one call and the apply behind it is not.
				if detailed && !dryRun {
					probe := append(slices.Clone(args), "--dry-run")

					out, err := tal.Combined(probe...)
					if err != nil || dryRunChanged(out) {
						// A dry run talman could not read is a change: for a
						// gate that decides whether something happened,
						// "cannot tell" has to mean "assume it did".
						markChanged()
					}
				}

				// Captured when the output has to be read as well as shown:
				// several nodes in flight, or a dry run whose answer is the
				// exit code.
				if grouped || (detailed && dryRun) {
					out, err := tal.Combined(args...)

					if err != nil {
						say(header + string(out) + "   error: " + err.Error() + "\n")

						return err
					}

					say(header + string(out))

					if detailed && dryRun && dryRunChanged(out) {
						markChanged()
					}
				} else {
					say(header)

					if err := tal.Stream(args...); err != nil {
						return err
					}
				}

				// Nothing to wait for when the change was not enacted: a dry
				// run touches nothing, and staged config lands on the next
				// reboot rather than now.
				if dryRun || mode == "staged" {
					return nil
				}

				if wait {
					// A node adopted out of maintenance mode installs Talos,
					// reboots, and then waits for a cluster to join. Before
					// `talman bootstrap` there is no cluster and no etcd, so
					// its API never comes back and waiting for it is a timeout
					// with extra steps -- ten silent minutes per node, on the
					// one path where every node is in that state.
					//
					// Bootstrap runs after the configs are applied, by design,
					// so this is the ordinary shape of building a cluster
					// rather than a mistake to report.
					if maintenance && !clusterBootstrapped() {
						say(fmt.Sprintf("   not waiting for %s: nothing to join until `talman bootstrap` runs\n",
							n.Hostname))

						bootstrapMu.Lock()
						adoptedEarly++
						bootstrapMu.Unlock()

						// Its state is whatever the install makes of it, which
						// talman did not watch: forget the reading rather than
						// leave a stale one for the health gate.
						modesMu.Lock()
						delete(modes, n.IPAddress)
						modesMu.Unlock()

						return nil
					}

					logf := func(format string, args ...any) {
						say(fmt.Sprintf(format+"\n", args...))
					}

					if err := tal.WaitReady(tc, n.IPAddress, stabilize, timeout, logf); err != nil {
						return fmt.Errorf("%w\n  re-run once it recovers, "+
							"or pass --wait=false to roll on regardless", err)
					}

					// It answered on the secure API, so whatever it was
					// before this, it is in the cluster now.
					modesMu.Lock()
					modes[n.IPAddress] = talosctl.ModeRunning
					modesMu.Unlock()
				} else {
					// Without the wait talman did not see what the node did
					// next, and the answer it has is now stale -- a node
					// recorded as being in maintenance mode keeps the health
					// gate down for the whole rest of the run. Forget it, so
					// the gate asks again rather than trusting a reading from
					// before the config landed.
					modesMu.Lock()
					delete(modes, n.IPAddress)
					modesMu.Unlock()
				}

				return nil
			}

			// Control planes one at a time, workers up to --parallel: the
			// unit of risk is still a node, and a batch of workers cannot
			// take out a cluster the way two control planes rebooting
			// together can.
			done := 0

			// Nothing is enacted by a dry run or a staged apply, so the rule
			// that keeps control planes apart has nothing to protect: they
			// batch with everything else: asking fifty nodes what would
			// change should not take fifty turns.
			inert := dryRun || mode == "staged"

			if inert && !cmd.Flags().Changed("parallel") {
				parallel = defaultParallel
			}

			var (
				okMu      sync.Mutex
				succeeded = map[string]bool{}
			)

			for _, batch := range batchesFor(targets, parallel, inert) {
				grouped := len(batch) > 1

				if _, err := eachNode(batch, len(batch), func(n *config.Node) (struct{}, error) {
					if err := applyOne(n, grouped); err != nil {
						return struct{}{}, err
					}

					okMu.Lock()
					succeeded[n.IPAddress] = true
					okMu.Unlock()

					return struct{}{}, nil
				}); err != nil {
					return fmt.Errorf("%w\n%s", err,
						resumeHint("apply", "applied", without(targets[done:], succeeded)))
				}

				done += len(batch)

				if dryRun || mode == "staged" {
					continue
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
					if outside := notInCluster(cfg, tal, tc, modes, parallel); len(outside) > 0 {
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

					if err := clusterHealth(cfg, tal, tc, timeout); err != nil {
						return fmt.Errorf("cluster is unhealthy after applying to %s: %w\n%s",
							names(batch), err, resumeHint("apply", "applied", targets[done:]))
					}
				}
			}

			if adoptedEarly > 0 {
				fmt.Fprintf(os.Stderr, "%d node(s) adopted; they finish joining once the cluster exists\n"+
					"  talman bootstrap   next, then `talman health`\n", adoptedEarly)
			}

			if detailed && changed {
				return errChanged
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

	addParallelFlag(cmd, &parallel, 1,
		"how many nodes to work on at once; control planes go one at a time unless nothing is enacted")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"ask each node what the config would change, and change nothing")
	addDetailedExitCode(cmd, &detailed)

	// Rolling straight on to the next node is how a bad config takes out a
	// whole control plane instead of one machine, so both gates default on.
	// Neither runs for a dry run, which enacts nothing to wait for.
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
func newNodes(tal *talosctl.Runner, talosconfig string, targets []*config.Node, parallel int) ([]*config.Node, error) {
	// Asked concurrently: this sweep touches every target before anything
	// happens, and a node that is down costs two dial timeouts to establish
	// that. Serially, one dead machine delayed the whole run by a minute.
	states, err := eachNode(targets, parallel, func(n *config.Node) (talosctl.Mode, error) {
		return tal.Mode(talosconfig, n.IPAddress), nil
	})
	if err != nil {
		return nil, err
	}

	out := make([]*config.Node, 0, len(targets))

	for i, n := range targets {
		switch states[i] {
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
	modes map[string]talosctl.Mode, parallel int,
) []string {
	unknown := make([]*config.Node, 0, len(cfg.Nodes))

	for i := range cfg.Nodes {
		if _, known := modes[cfg.Nodes[i].IPAddress]; !known {
			unknown = append(unknown, &cfg.Nodes[i])
		}
	}

	// Concurrently, and once: the gate runs between batches, so a node
	// answered for here is not asked again for the rest of the roll-out.
	states, _ := eachNode(unknown, parallel, func(n *config.Node) (talosctl.Mode, error) {
		return tal.Mode(talosconfig, n.IPAddress), nil
	})

	for i, n := range unknown {
		modes[n.IPAddress] = states[i]
	}

	var outside []string

	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]

		if modes[n.IPAddress] != talosctl.ModeRunning {
			outside = append(outside, fmt.Sprintf("%s is %s", n.Hostname, modes[n.IPAddress]))
		}
	}

	return outside
}

// dryRunChanged reads talosctl's dry run for whether the node would change.
//
// Talos computes the diff on the node and prints "Config diff: No changes."
// when there is none, which is the only part of that output talman depends on.
// Anything it cannot find that line in counts as changed: a CI job asking
// "did something change?" is better told yes it did when talman cannot tell
// than no it did not.
func dryRunChanged(out []byte) bool {
	return !strings.Contains(string(out), "No changes.")
}

// clusterHealth runs the same check `talman health` performs, from the first
// control plane in the config.
//
// Not from the node just applied: a worker cannot answer for the cluster, and
// picking whichever node the roll-out happens to be on would make a failing
// gate mean something different at each step.
func clusterHealth(cfg *config.Config, tal *talosctl.Runner, talosconfig string, waitFor time.Duration) error {
	from, err := healthNode(cfg, tal, talosconfig)
	if err != nil {
		return err
	}

	// Client-side: the gate is "can talman still see a healthy cluster from
	// here", which is the question a roll-out has to stop on.
	return tal.Stream(healthArgs(cfg, talosconfig, from, false, waitFor)...)
}

// renderForApply re-renders the targeted nodes and writes them out, so what
// gets applied is what the config and patches currently say.
//
// The talosconfig is regenerated too: it is the credential the apply itself
// uses, and leaving it behind while the machine configs move forward is how a
// cluster ends up unreachable by its own tooling.
func renderForApply(cfg *config.Config, targets []*config.Node, parallel int) error {
	r, err := newRenderer(cfg, false, os.Stderr)
	if err != nil {
		return err
	}
	defer r.Close()

	// Rendering is the same local work `talman render` parallelises, and it
	// all happens before the first node is touched: an apply that staggers
	// its reboots has no reason to stagger its gen config calls too.
	results, err := eachNode(targets, parallel, func(n *config.Node) (*render.Result, error) {
		res, err := r.Node(n)
		if err != nil {
			return nil, err
		}

		if err := r.Validate(res, cfg.TalosMode); err != nil {
			return nil, err
		}

		return res, nil
	})
	if err != nil {
		return err
	}

	return r.WriteAll(results, true)
}
