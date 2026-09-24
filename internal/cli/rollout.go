package cli

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
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
}

// one does a single node's work. grouped says its output is captured with a
// batch's rather than streamed, and acted whether it changed anything, which
// is what decides if the health gate has something to check after the batch.
type one func(n *config.Node, grouped bool, say func(string)) (acted bool, err error)

func (ro rollOut) run(do one) error {
	rec := currentRun

	parallel := ro.parallel

	// Nothing to stagger, so no reason to keep nodes apart -- but the same
	// bound on processes in flight a read-only pass keeps.
	if ro.inert && !ro.cmd.Flags().Changed("parallel") {
		parallel = defaultParallel
	}

	var (
		done      int
		mu        sync.Mutex
		succeeded = map[string]bool{}
	)

	for _, batch := range batchesFor(ro.targets, parallel, ro.inert) {
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

		// After a batch that changed something, and never after the last:
		// a batch that skipped every node left nothing new to check, and
		// after the last there is nothing left for the gate to protect.
		if ro.health && !ro.inert && acted && done < len(ro.targets) {
			fmt.Fprintf(os.Stderr, "   checking cluster health before continuing\n")

			if err := clusterHealth(ro.cfg, ro.tal, ro.tc, ro.timeout); err != nil {
				return fmt.Errorf("cluster is unhealthy after %s %s: %w\n%s",
					gerund(ro.verb), names(batch), err,
					resumeHint(ro.verb, ro.done, ro.targets[done:], replayFlags(ro.cmd, "node")...))
			}
		}
	}

	return nil
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
