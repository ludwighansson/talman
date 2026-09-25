package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/redact"
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
		waves      waveFlags
		mode       string
		insecure   bool
		onlyNew    bool
		onboard    bool
		bootstrap  bool
		yes        bool
		parallel   int
		detailed   bool
		diff       bool
		hideSecret bool
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
		Long: `Apply renders each selected node's config and sends it, one node at a time,
waiting for each to come back before the next. Control planes always go
alone; --parallel batches workers.

New nodes, in maintenance mode, are configured only with --onboard-new-nodes
or --only-new-nodes: the maintenance service authenticates nothing, and a
config carries the CA keys. --bootstrap builds a new cluster: the first
control plane, etcd on it, then the rest.

--dry-run asks each node what would change, --diff prints that before
applying, and --detailed-exit-code turns it into an exit code (2 changed, 0
unchanged, 1 error). Printed diffs have this cluster's secrets redacted.

More in the README: "Onboarding new nodes", "Rolling changes out safely" and
"Exit codes for CI".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec := currentRun

			// A drift check and a roll-out of the same cluster are
			// different jobs, and neither's metrics should replace the
			// other's.
			if dryRun {
				rec.SetLabel("dry_run", "true")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			if mode == "reboot" {
				return errors.New("--mode=reboot was removed in Talos 1.14; " +
					"use --mode=auto (apply now, reboot only if required) or --mode=staged")
			}

			if !slices.Contains(applyModes, mode) {
				return fmt.Errorf("--mode %q is invalid: must be one of %s",
					mode, strings.Join(applyModes, ", "))
			}

			targets, err := selectNodes(cfg, nodes)
			if err != nil {
				return err
			}

			if bootstrap {
				if err := bootstrapFlagsAllowed(cmd, mode, onlyNew, wait); err != nil {
					return err
				}

				// A cluster being built is made of nodes in maintenance
				// mode: onboarding them is the point.
				onboard = true
			}

			// The waves first, so a mistyped --from fails before the bundle
			// is decrypted, and only the nodes in the selected waves are
			// rendered. Planned again after --only-new-nodes, below.
			if _, targets, _, err = waves.plan(cfg, targets); err != nil {
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
			secrets := func() (*redact.Redactor, error) { return render.Secrets(cfg) }

			if !noRender {
				if secrets, err = renderForApply(cfg, targets, parallel); err != nil {
					return err
				}
			}

			// Talos' diff is a diff of the machine config, so it carries what
			// the machine config carries: join tokens, the cluster secret,
			// the machine CA. talman knows those values -- it decrypted them
			// and rendered them into the config it is sending -- so it can
			// take them back out of anything it prints.
			//
			// Settled before any node is touched, so the answer cannot change
			// half way through a run, and so that a diff that cannot be
			// hidden is refused before a config it was meant to preview is
			// sent.
			redactor, withheld, err := redaction(hideSecret, diff, dryRun, noRender, secrets)
			if err != nil {
				return err
			}

			// show is what may be printed of what talosctl said. When talman
			// cannot hide the secrets in a dry run, the diff itself is kept
			// back and only its answer printed: the exit code still means
			// what it says, so a drift check without the SOPS key keeps
			// working, and nothing it cannot vouch for reaches a log.
			show := func(out []byte, failed bool) []byte {
				switch {
				case !withheld:
					return redactor.Bytes(out)
				case failed:
					return []byte("output not shown\n")
				case dryRunChanged(out):
					return []byte("changes; diff not shown\n")
				default:
					return []byte("no changes\n")
				}
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)

			// Whether there is a cluster: --bootstrap builds one, and needs
			// there to be none yet; every other apply configures one, and a
			// cluster not yet bootstrapped is one whose nodes cannot finish
			// joining it.
			states := probeControlPlanes(cfg, tal, tc)

			switch {
			case bootstrap:
				configured, err := checkBootstrappable(states)
				if err != nil {
					return err
				}

				if !dryRun {
					if err := confirmConfigured(cfg, configured, yes); err != nil {
						return err
					}
				}
			case allNew(states):
				return fmt.Errorf("every control plane is new, in maintenance mode: this cluster has not been "+
					"built yet\n  %s", talmanCmd("apply --bootstrap"))
			case noEtcd(states):
				// Never bootstrapped, or etcd broken: talman cannot tell, and
				// refusing would stand in the way of the apply that fixes a
				// broken one.
				fmt.Fprintf(os.Stderr, "warning: no control plane runs etcd; if this cluster was never "+
					"bootstrapped:\n  %s\n", talmanCmd("apply --bootstrap"))
			}

			// An explicit --insecure (or --insecure=false) is an instruction,
			// not a hint: honour it for every node and skip the probing.
			forced := cmd.Flags().Changed("insecure")

			// New nodes found before anything is sent, not when the roll-out
			// reaches them: by then the nodes before them have been applied
			// and rebooted, and the run stops halfway.
			if !onboard && !onlyNew && !forced {
				if err := refuseNewNodes(tal, tc, targets, states); err != nil {
					return err
				}
			}

			// What each node answered, so the health gate can tell a cluster
			// that is still being built from one that is misbehaving.
			modes := map[string]talosctl.Mode{}

			// Why talman did less than usual is said once, at the end: the
			// reason does not change between nodes, and repeating it under
			// each one buries the roll-out in its own footnotes.
			var (
				saidUngated bool
				ungated     int
				gated       int
			)

			if onlyNew {
				targets, err = newNodes(tal, tc, targets, parallel)
				if err != nil {
					return err
				}

				if len(targets) == 0 {
					fmt.Fprintf(os.Stderr, "nothing to onboard: no selected node is new (in maintenance mode)\n")
					rec.SetChanged(false)

					return nil
				}
			}

			// After --only-new-nodes, so the waves hold only the nodes it kept.
			stages, targets, thenFrom, err := waves.plan(cfg, targets)
			if err != nil {
				return err
			}

			for _, n := range targets {
				rec.Plan(n.Hostname, string(n.Role))
			}

			// Everything the per-node pass touches is per node, except the
			// probe cache, which several nodes in a batch write at once.
			var modesMu sync.Mutex

			// Guards the answer --detailed-exit-code reports, which several
			// nodes in a batch may reach at once.
			var (
				changedMu sync.Mutex
				changed   bool
			)

			// Set once etcd has been bootstrapped by this run: a node that
			// already had its config sat quiet with no cluster to join, and
			// is waited for now that there is one.
			var bootstrapped atomic.Bool

			markChanged := func() {
				changedMu.Lock()
				defer changedMu.Unlock()

				changed = true
			}

			// Nodes that answered, when asked first, that the config changes
			// nothing: all the roll-out's soak and health gate need to know,
			// since a wave of those left nothing to watch.
			var (
				unchangedMu sync.Mutex
				unchanged   = map[string]bool{}
			)

			// Asked first too when the answer decides whether to soak or
			// gate: a dry run per node is cheap next to a ten-minute soak
			// after a wave that changed nothing.
			wantsAnswer := detailed || rec != nil || health || cfg.Rollout.SoakDuration() > 0

			applyOne := func(n *config.Node, position int, say func(string)) error {
				file := cfg.MachineConfigPath(n)

				if _, err := os.Stat(file); err != nil {
					return fmt.Errorf("no rendered config for %s at %s: run `%s` first",
						n.Hostname, file, talmanCmd("render"))
				}

				// A cluster is rarely all one thing: after a reset, or when a
				// machine is added, some nodes answer with cluster PKI and
				// some only on the maintenance service. Asking each node
				// which it is costs one call and is the difference between
				// onboarding a node and failing the run.
				maintenance := insecure

				if !forced {
					state := tal.Mode(tc, n.IPAddress)

					modesMu.Lock()
					modes[n.IPAddress] = state
					modesMu.Unlock()

					switch state {
					case talosctl.ModeMaintenance:
						// Unauthenticated: a reused address, a spoofed host or
						// a passing TLS failure on a real node all look like
						// this, and each would be handed the cluster's keys.
						if !onboard && !onlyNew {
							return fmt.Errorf("%s (%s) is a new node: it answers only the maintenance service, "+
								"which authenticates nothing, so it gets its config, CA keys included, only when "+
								"asked\n  %s", n.Hostname, n.IPAddress, talmanCmd("apply --onboard-new-nodes -n "+n.Hostname))
						}

						maintenance = true
					case talosctl.ModeRunning:
						maintenance = false
					case talosctl.ModeUnreachable:
						// A node that was given its config before the cluster
						// existed sits quiet until there is one to join. Once
						// this run has bootstrapped it, that node is on its
						// way, and is waited for rather than given up on.
						if !bootstrapped.Load() {
							return fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service",
								n.Hostname, n.IPAddress)
						}

						say(fmt.Sprintf("== %s (%s) has its config; waiting for it to join the cluster\n",
							n.Hostname, n.IPAddress))

						logf := detailLog(say)

						if err := tal.WaitReady(tc, n.IPAddress, stabilize, timeout, logf); err != nil {
							return err
						}

						modesMu.Lock()
						modes[n.IPAddress] = talosctl.ModeRunning
						modesMu.Unlock()
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

				var note string

				switch {
				case maintenance && forced:
					note = " · through the maintenance service (--insecure)"
				case maintenance:
					note = " · onboarding"
				}

				header := fmt.Sprintf("== [%d/%d] %s (%s)%s\n",
					position, len(targets), n.Hostname, n.IPAddress, note)

				// A real apply says nothing about whether the config it sent
				// differed from the one already there, and Talos will only
				// say so if asked in advance. Both --diff and
				// --detailed-exit-code want that answer, so it is asked for
				// once and used for both: the dry run is one call, and the
				// apply behind it is not.
				block := header

				// Metrics ask too: "changed" is what a CI alert most often
				// wants to know, and without asking it is not known.
				if askFirst(dryRun, wantsAnswer, diff) {
					probe := append(slices.Clone(args), "--dry-run")

					out, err := tal.Combined(probe...)
					if err == nil {
						rec.NodeChanged(n.Hostname, dryRunChanged(out))

						if !dryRunChanged(out) {
							unchangedMu.Lock()
							unchanged[n.IPAddress] = true
							unchangedMu.Unlock()
						}
					}

					if detailed && (err != nil || dryRunChanged(out)) {
						// A dry run talman could not read is a change: for a
						// gate that decides whether something happened,
						// "cannot tell" has to mean "assume it did".
						markChanged()
					}

					if diff {
						// Asked to see the change before it is made, and
						// talman could not see it: sending the config anyway
						// is exactly what the flag was there to prevent.
						if err != nil {
							say(block + indent(show(out, true)) +
								detail + "error: asking what would change: " + err.Error() + "\n")

							return fmt.Errorf("%s (%s): could not show what would change, so changed nothing: %w",
								n.Hostname, n.IPAddress, err)
						}

						block += indent(show(out, false))
					}
				}

				// Captured rather than streamed, so the node's heading and
				// what talosctl said under it arrive together: eight nodes in
				// flight would otherwise interleave, and one node's apply
				// returns in a breath anyway.
				out, err := tal.Combined(args...)
				changes := dryRun && dryRunChanged(out)

				if dryRun && err == nil {
					rec.NodeChanged(n.Hostname, changes)
				}

				out = show(out, err != nil)

				if err != nil {
					say(block + indent(out) + detail + "error: " + err.Error() + "\n")

					return err
				}

				say(block + indent(out))

				if detailed && changes {
					markChanged()
				}

				// Nothing to wait for when the change was not enacted: a dry
				// run touches nothing, and staged config lands on the next
				// reboot rather than now.
				if dryRun || mode == "staged" {
					return nil
				}

				if wait {
					logf := detailLog(say)

					if err := tal.WaitReady(tc, n.IPAddress, stabilize, timeout, logf); err != nil {
						return fmt.Errorf("%w\n  %s", err, waitFailedHint(targets))
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

			// Nothing is enacted by a dry run or a staged apply, so the rule
			// that keeps control planes apart has nothing to protect: they
			// batch with everything else, and asking fifty nodes what would
			// change should not take fifty turns.
			inert := dryRun || mode == "staged"

			// [n/m] is the node's place in the run, not the order it
			// finished in: a batch of workers reports as it goes.
			positions := make(map[string]int, len(targets))
			for i, n := range targets {
				positions[n.IPAddress] = i + 1
			}

			// The gate checks the cluster the config describes, so it only
			// means something once that cluster exists. A node that has not
			// been onboarded yet answers with a self-signed maintenance
			// certificate, which the check reports as "certificate signed by
			// unknown authority" -- a build-out step read as a broken
			// cluster. So while any node is outside the cluster, the gate
			// stands down and says which nodes those are. Once they have all
			// joined it gates every node, which is the roll-out case it
			// exists for.
			standDown := func() bool {
				outside := notInCluster(cfg, tal, tc, modes, parallel)
				if len(outside) == 0 {
					gated++

					return false
				}

				ungated++

				// Which ones, for whoever is asking why; the summary at the
				// end gives the count.
				if opts.verbose && !saidUngated {
					saidUngated = true

					fmt.Fprintf(os.Stderr, "%s%s\n", detail, strings.Join(outside, ", "))
				}

				return true
			}

			perNode := func(n *config.Node, _ bool, say func(string)) (bool, error) {
				err := applyOne(n, positions[n.IPAddress], say)

				unchangedMu.Lock()
				defer unchangedMu.Unlock()

				return !unchanged[n.IPAddress], err
			}

			// Control planes one at a time, workers up to --parallel: the
			// unit of risk is still a node, and a batch of workers cannot
			// take out a cluster the way two control planes rebooting
			// together can. A node counts as acting for the gate unless it
			// was asked first and answered that nothing changes.
			ro := rollOut{
				cmd: cmd, cfg: cfg, tal: tal, tc: tc,
				verb: "apply", done: "applied",
				targets: targets, parallel: parallel, inert: inert,
				health: health, timeout: timeout,
				// auto is the one mode that may reboot a node.
				waits:     wait || mode != "auto",
				standDown: standDown,
				stages:    stages, soak: cfg.Rollout.SoakDuration(), thenFrom: thenFrom,
			}

			if bootstrap {
				// The first control plane first, alone, then etcd on it;
				// then everything else, into a cluster that exists.
				first := cfg.ControlPlanes()[0]

				if dryRun {
					rest := make([]*config.Node, 0, len(targets))
					for _, n := range targets {
						if n != first {
							rest = append(rest, n)
						}
					}

					stages, _, _, err := waves.plan(cfg, rest)
					if err != nil {
						return err
					}

					if len(stages) == 0 && len(rest) > 0 {
						stages = []config.Staged{{Nodes: rest}}
					}

					printBootstrapPlan(tal, tc, first, stages)

					return nil
				}

				if err := bootstrapFirst(cmd, tal, tc, first, timeout, func(say func(string)) error {
					_, err := perNode(first, false, say)

					return err
				}); err != nil {
					return err
				}

				bootstrapped.Store(true)

				rest := make([]*config.Node, 0, len(targets)-1)
				for _, n := range targets {
					if n != first {
						rest = append(rest, n)
					}
				}

				// A resume carries on in the cluster this run made: it is
				// not a second bootstrap, and its nodes are still onboarded.
				if ro.stages, ro.targets, ro.thenFrom, err = waves.plan(cfg, rest); err != nil {
					return err
				}

				ro.hintDrop = []string{"bootstrap"}
				ro.hintAdd = []string{"--onboard-new-nodes"}
			}

			if len(ro.targets) > 0 {
				if err := ro.run(perNode); err != nil {
					return err
				}
			}

			if summary := gateSummary(ungated, gated); summary != "" {
				fmt.Fprintf(os.Stderr, "\n%s\n", summary)
			}

			if mode == "staged" && !dryRun && len(targets) > 0 {
				fmt.Fprintf(os.Stderr, "\nconfigs staged; they take effect on the next reboot → talman%s reboot%s\n",
					configArg(), nodeArgs(targets, len(cfg.Nodes)))
			}

			if detailed && changed {
				return errChanged
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	addWaveFlags(cmd, &waves)
	cmd.Flags().StringVarP(&mode, "mode", "m", "auto",
		"apply mode: "+strings.Join(applyModes, ", "))
	cmd.Flags().BoolVarP(&insecure, "insecure", "i", false,
		"force the maintenance service for every node (default: ask each node which API it answers)")
	cmd.Flags().BoolVar(&onboard, "onboard-new-nodes", false,
		"configure new nodes (in maintenance mode) too, over the unauthenticated maintenance service")
	cmd.Flags().BoolVar(&bootstrap, "bootstrap", false,
		"build a new cluster: configure the first control plane, bootstrap etcd on it, then the rest")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false,
		"with --bootstrap, skip the question asked when control planes are configured but run no etcd")
	cmd.Flags().BoolVar(&onlyNew, "only-new-nodes", false,
		"restrict the run to new nodes (in maintenance mode), and onboard them")

	addParallelFlag(cmd, &parallel, 1,
		"how many nodes to work on at once; control planes go one at a time unless nothing is enacted")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"ask each node what the config would change, and change nothing")
	cmd.Flags().BoolVar(&diff, "diff", false,
		"print what each node would change before changing it (implied by --dry-run)")
	cmd.Flags().BoolVar(&hideSecret, "redact-secrets", true,
		"replace this cluster's own secrets with [redacted] in printed diffs")
	addDetailedExitCode(cmd, &detailed)

	// Rolling straight on to the next node while the last is still rebooting
	// is how a control plane loses quorum, so the wait defaults on: it asks
	// only the node itself, and a cluster in trouble does not stop it. The
	// health gate asks the whole cluster, so an unhealthy one fails it after
	// the first node -- including the apply meant to fix it -- and it is
	// opt-in. Neither runs for a dry run, which enacts nothing to wait for.
	cmd.Flags().BoolVar(&wait, "wait", true,
		"wait for each node to come back before applying to the next")
	cmd.Flags().BoolVar(&health, "health", false,
		"run a cluster health check between nodes, and stop if it fails")
	cmd.Flags().DurationVar(&stabilize, "stabilize", 30*time.Second,
		"how long a node must stay reachable before it counts as back")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute,
		"how long to wait for a node to come back, and for the health check between nodes")
	cmd.Flags().BoolVar(&noRender, "no-render", false,
		"apply the configs already in the output directory instead of re-rendering")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// newNodes keeps the targets that are in maintenance mode, which is what
// "not onboarded yet" looks like from outside.
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
				"--only-new-nodes cannot tell whether it needs onboarding\n"+
				"  bring it up, or narrow the run with -n", n.Hostname, n.IPAddress)
		}
	}

	return out, nil
}

// notInCluster describes the configured nodes that are not part of the cluster
// yet, empty when every one of them is.
//
// Nodes this run has already asked about are not asked again: the answers are
// carried in modes, including for a node this run just onboarded and watched
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

// gateSummary says what a run did less of, once and at the end.
//
// Accurately, which is the whole difficulty: a gate that stood down for the
// first step and ran for the rest is not a run that skipped the gate, and a
// summary claiming otherwise is worse than no summary. Empty when there is
// nothing to report.
func gateSummary(ungated, gated int) string {
	switch {
	case ungated > 0 && gated == 0:
		return "not gating on health: nodes not in the cluster yet"
	case ungated > 0:
		return fmt.Sprintf("health gate stood down for %d of %d step(s): nodes not in the cluster yet",
			ungated, ungated+gated)
	default:
		return ""
	}
}

// detail is the indent of what is said under a node's heading. The detail is
// talosctl's, indented to read as detail: the heading says which node, the
// lines below it say what that node's talosctl said.
const detail = "     "

// indent puts detail's prefix on every line of a command's output.
func indent(out []byte) string {
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = detail + line
	}

	return strings.Join(lines, "\n") + "\n"
}

// askFirst reports whether a node should be asked what would change before it
// is told to change.
//
// One call answers both questions. A dry run is already that question, so it
// asks nothing extra; an apply asks when something wants the answer, and the
// answer then serves whichever of them asked.
func askFirst(dryRun, detailed, diff bool) bool {
	return !dryRun && (detailed || diff)
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
//
// It returns what the pass knows about the secrets it rendered in, for hiding
// them in anything printed afterwards.
func renderForApply(cfg *config.Config, targets []*config.Node, parallel int,
) (func() (*redact.Redactor, error), error) {
	r, err := newRenderer(cfg, false, os.Stderr)
	if err != nil {
		return nil, err
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

		if err := r.Validate(res, cfg.ValidationModeFor(res.Node)); err != nil {
			return nil, err
		}

		return res, nil
	})
	if err != nil {
		return nil, err
	}

	if err := r.WriteAll(results, true); err != nil {
		return nil, err
	}

	redactor, secretsErr := r.Secrets()

	return func() (*redact.Redactor, error) { return redactor, secretsErr }, nil
}

// redaction settles how an apply hides secrets in what it prints: the
// redactor to use, or whether diffs are to be withheld altogether.
//
// Only a run that prints a config needs to know, and only one that did not
// just render has anything to pay for the answer: a plain --no-render apply
// is not made to decrypt a bundle for nothing. When the secrets cannot be
// known, a --diff is refused outright -- it asked to see the change before
// it is made -- while a dry run keeps its answer and loses only the diff.
func redaction(hide, diff, dryRun, noRender bool, secrets func() (*redact.Redactor, error),
) (*redact.Redactor, bool, error) {
	prints := diff || dryRun

	if !hide || (noRender && !prints) {
		return nil, false, nil
	}

	redactor, err := secrets()
	if err == nil {
		return redactor, false, nil
	}

	if !prints {
		return nil, false, nil
	}

	err = fmt.Errorf("cannot hide this cluster's secrets in the diff: %w\n"+
		"  pass --redact-secrets=false to print it with them in", err)

	if diff {
		return nil, false, err
	}

	fmt.Fprintf(os.Stderr, "%v\n  diffs are not shown\n", err)

	return nil, true, nil
}

// waitFailedHint is what to do about a node that did not come back. Rolling
// on without the wait is only offered where talman would allow it: not for a
// run reaching more than one control plane, where it is refused.
func waitFailedHint(targets []*config.Node) string {
	if waitsForControlPlanes(targets) != nil {
		return "re-run once it recovers, or pass --timeout to wait longer"
	}

	return "re-run once it recovers, or pass --wait=false to roll on regardless"
}

// detailLog prints a progress line under a node's heading, as the detail it
// is.
func detailLog(say func(string)) func(string, ...any) {
	return func(format string, args ...any) {
		say(detail + strings.TrimLeft(fmt.Sprintf(format+"\n", args...), " "))
	}
}
