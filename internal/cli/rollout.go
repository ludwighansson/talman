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
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// rollOut is a node-by-node pass that changes a cluster -- upgrade, reboot --
// with the rules they share: control planes alone, workers batched up to
// --parallel, a stop at the first failure naming what is left, and the health
// gate between batches.
type rollOut struct {
	cmd *cobra.Command
	cfg *config.Config
	tal *talosctl.Runner
	tc  string

	// verb and done name the operation in resume hints: "upgrade",
	// "upgraded".
	verb, done string

	targets  []*config.Node
	parallel int
	// inert is a pass that enacts nothing, a dry run: control planes need
	// not go alone, and there is no health to gate on.
	inert   bool
	health  bool
	timeout time.Duration
	// waits is whether each node's command returns only once the node is
	// back. Without it the next control plane would start while the last
	// is still down.
	waits bool

	// standDown, when set, is asked before each health gate, and a true
	// answer skips it: apply's gate means nothing while nodes it has not
	// adopted yet are still outside the cluster.
	standDown func() bool

	// stages are the targets in the config's rollout waves; nil is one
	// stage of every target, in config order. soak is the wait after each
	// wave that did something.
	stages []config.Staged
	soak   time.Duration
	// thenFrom is the wave --until stopped short of, for the hint that
	// carries on from it.
	thenFrom string
}

// one does a single node's work. grouped says its output is captured with a
// batch's rather than streamed, and acted whether it changed anything, which
// is what decides if the health gate has something to check after the batch.
type one func(n *config.Node, grouped bool, say func(string)) (acted bool, err error)

func (ro rollOut) run(do one) error {
	rec := currentRun

	if !ro.inert && !ro.waits {
		if err := waitsForControlPlanes(ro.targets); err != nil {
			return err
		}
	}

	parallel := ro.parallel

	// Nothing to stagger, so no reason to keep nodes apart -- but the same
	// bound on processes in flight a read-only pass keeps.
	if ro.inert && !ro.cmd.Flags().Changed("parallel") {
		parallel = defaultParallel
	}

	stages := ro.stages
	if len(stages) == 0 {
		stages = []config.Staged{{Nodes: ro.targets}}
	}

	// In the order the stages put them, which is what a resume hint counts
	// through.
	ro.targets = nil
	for _, st := range stages {
		ro.targets = append(ro.targets, st.Nodes...)
	}

	var (
		done      int
		mu        sync.Mutex
		succeeded = map[string]bool{}
	)

	gate := func(after []*config.Node) error {
		if ro.standDown != nil && ro.standDown() {
			return nil
		}

		fmt.Fprintf(os.Stderr, "%schecking cluster health before continuing\n", detail)

		if err := clusterHealth(ro.cfg, ro.tal, ro.tc, ro.timeout); err != nil {
			return fmt.Errorf("cluster is unhealthy after %s %s: %w\n%s",
				gerund(ro.verb), names(after), err,
				resumeHint(ro.verb, ro.done, ro.targets[done:], replayFlags(ro.cmd, "node")...))
		}

		return nil
	}

	for si, st := range stages {
		waves := len(stages) > 1 || st.Wave != nil
		if waves {
			fmt.Fprintf(os.Stderr, "== wave %d/%d: %s, %d node(s)\n", si+1, len(stages), st.Name(), len(st.Nodes))
		}

		batches := batchesFor(st.Nodes, parallel, ro.inert)
		actedInWave := false

		for bi, batch := range batches {
			out := newInOrder(os.Stderr, batch)
			grouped := len(batch) > 1
			acted := false

			if _, err := eachNode(batch, len(batch), func(n *config.Node) (struct{}, error) {
				defer out.finish(n)

				rec.NodeStart(n.Hostname, string(n.Role))

				did, err := do(n, grouped, func(s string) { out.say(n, s) })
				rec.NodeDone(n.Hostname, err)

				if err != nil {
					return struct{}{}, err
				}

				mu.Lock()
				succeeded[n.IPAddress] = true
				acted = acted || did
				mu.Unlock()

				return struct{}{}, nil
			}); err != nil {
				return fmt.Errorf("%w\n%s", err,
					resumeHint(ro.verb, ro.done, without(ro.targets[done:], succeeded),
						replayFlags(ro.cmd, "node")...))
			}

			done += len(batch)
			actedInWave = actedInWave || acted

			// Nothing is gated after the last node: by then there is
			// nothing left for the gate to protect.
			if ro.inert || done == len(ro.targets) {
				continue
			}

			// Inside a wave, after a batch that changed something: a batch
			// that skipped every node left nothing new to check.
			if bi < len(batches)-1 {
				if ro.health && acted {
					if err := gate(batch); err != nil {
						return err
					}
				}

				continue
			}

			// Between waves, after one that changed something: soak, then
			// gate -- a canary is only worth anything if the gate looks after
			// it has had time to go wrong. A wave that changed nothing has
			// nothing to watch.
			if !actedInWave {
				continue
			}

			if ro.soak > 0 {
				fmt.Fprintf(os.Stderr, "%swave %s done; soaking %s before the next\n", detail, st.Name(), ro.soak)

				if err := interrupt.Sleep(ro.soak); err != nil {
					return fmt.Errorf("stopped soaking after wave %s: %w\n%s", st.Name(), err,
						resumeHint(ro.verb, ro.done, ro.targets[done:], replayFlags(ro.cmd, "node")...))
				}
			}

			if ro.health {
				if err := gate(st.Nodes); err != nil {
					return err
				}
			}
		}
	}

	if ro.thenFrom != "" {
		fmt.Fprintf(os.Stderr, "\nstopped before wave %s, as --until asked\n  continue with: talman %s --from %s%s\n",
			ro.thenFrom, ro.verb, ro.thenFrom, resumeFlags(ro.cmd, "until"))
	}

	return nil
}

// resumeFlags is the command's flags for a hint that carries on with --from:
// everything kept as it was -- the node selection, --wave, --until -- but the
// --from the hint replaces, and whatever else drop names.
func resumeFlags(cmd *cobra.Command, drop ...string) string {
	flags := replayFlags(cmd, append([]string{"from"}, drop...)...)
	if len(flags) == 0 {
		return ""
	}

	return " " + strings.Join(flags, " ")
}

// runTalosctl runs one node's talosctl command: captured and printed whole
// under its header when grouped, streamed after it otherwise.
func runTalosctl(tal *talosctl.Runner, grouped bool, header string, say func(string), args []string) error {
	if !grouped {
		say(header)

		return tal.Stream(args...)
	}

	out, err := tal.Combined(args...)
	if err != nil {
		say(header + string(out) + "   error: " + err.Error() + "\n")

		return err
	}

	say(header + string(out))

	return nil
}

func gerund(verb string) string {
	switch verb {
	case "upgrade":
		return "upgrading"
	case "reboot":
		return "rebooting"
	default:
		return verb + "ing"
	}
}

// waitsForControlPlanes refuses a pass that would not wait for its nodes when
// it reaches more than one control plane.
//
// Going one at a time only keeps control planes apart if each command returns
// once its node is back. Without the wait, the next control plane starts while
// the last is still rebooting, which is the loss of quorum the one-at-a-time
// rule exists to prevent -- and a health gate run before the node has even
// gone down passes, and protects nothing.
func waitsForControlPlanes(targets []*config.Node) error {
	var cps []string

	for _, n := range targets {
		if n.IsControlPlane() {
			cps = append(cps, n.Hostname)
		}
	}

	if len(cps) < 2 {
		return nil
	}

	return fmt.Errorf("--wait=false with %d control planes selected (%s) would let them go down together "+
		"and lose etcd quorum: keep --wait, or select at most one control plane with -n",
		len(cps), strings.Join(cps, ", "))
}

// waveFlags narrow a roll-out to some of the config's waves. They select;
// they never reorder.
type waveFlags struct {
	only        []string
	from, until string
}

func addWaveFlags(cmd *cobra.Command, w *waveFlags) {
	cmd.Flags().StringSliceVar(&w.only, "wave", nil,
		"roll out only the waves holding these groups (repeatable; needs rollout: in the config)")
	cmd.Flags().StringVar(&w.from, "from", "", "start at the wave holding this group, skipping the ones before it")
	cmd.Flags().StringVar(&w.until, "until", "", "stop after the wave holding this group")

	for _, name := range []string{"wave", "from", "until"} {
		_ = cmd.RegisterFlagCompletionFunc(name, completeWaves)
	}
}

// plan lays targets out in the config's waves and keeps the ones the flags
// select. It returns the stages, the targets left in them, and the wave
// --until stopped short of, if any.
func (w waveFlags) plan(cfg *config.Config, targets []*config.Node) ([]config.Staged, []*config.Node, string, error) {
	set := len(w.only) > 0 || w.from != "" || w.until != ""

	if cfg.Rollout == nil {
		if set {
			return nil, nil, "", errors.New("--wave, --from and --until need rollout.waves in the config")
		}

		return nil, targets, "", nil
	}

	index := func(flag, name string) (int, error) {
		i, ok := cfg.Rollout.WaveOf(name)
		if !ok {
			return 0, fmt.Errorf("%s %s: no rollout wave holds the group %q", flag, name, name)
		}

		return i, nil
	}

	only := map[int]bool{}

	for _, name := range w.only {
		i, err := index("--wave", name)
		if err != nil {
			return nil, nil, "", err
		}

		only[i] = true
	}

	from, until := 0, len(cfg.Rollout.Waves)

	if w.from != "" {
		i, err := index("--from", w.from)
		if err != nil {
			return nil, nil, "", err
		}

		from = i
	}

	if w.until != "" {
		i, err := index("--until", w.until)
		if err != nil {
			return nil, nil, "", err
		}

		until = i
	}

	if from > until {
		return nil, nil, "", fmt.Errorf("--from %s comes after --until %s in rollout.waves, "+
			"which leaves nothing between them", w.from, w.until)
	}

	var (
		kept     []config.Staged
		left     []*config.Node
		thenFrom string
	)

	for _, st := range cfg.Stages(targets) {
		switch {
		case len(only) > 0 && !only[st.Index], st.Index < from:
			continue
		case st.Index > until:
			if thenFrom == "" {
				thenFrom = st.Ref()
			}

			continue
		}

		kept = append(kept, st)
		left = append(left, st.Nodes...)
	}

	// A selection that reaches no node is a mistake to report, not a run
	// that did nothing and succeeded.
	if len(left) == 0 && len(targets) > 0 {
		return nil, nil, "", errors.New("the selected waves hold none of the selected nodes")
	}

	return kept, left, thenFrom, nil
}

// completeWaves offers the groups the rollout's waves name.
func completeWaves(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.LoadNoValidate(config.FindConfig(opts.configFile))
	if err != nil || cfg.Rollout == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var out []string

	for i, w := range cfg.Rollout.Waves {
		for _, g := range w.Groups {
			out = append(out, fmt.Sprintf("%s\twave %d", g, i+1))
		}
	}

	return append(out, config.RestWave+"\tthe nodes no wave names"), cobra.ShellCompDirectiveNoFileComp
}
