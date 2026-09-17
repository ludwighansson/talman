package cli

import (
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
)

// defaultParallel is how many nodes a read-only pass works on at once.
//
// A fixed number rather than one derived from the CPU count: the work is
// waiting on talosctl processes and a cluster's API, not on this machine's
// cores, and a large cluster should not open fifty connections because the
// laptop rendering it happens to be a big one.
const defaultParallel = 8

// addParallelFlag registers --parallel with the given default.
func addParallelFlag(cmd *cobra.Command, target *int, def int, help string) {
	cmd.Flags().IntVarP(target, "parallel", "p", def, help)
}

// eachNode runs fn over the nodes, at most parallel at a time, and returns the
// results in the order the nodes were given rather than the order they
// finished -- a table that reshuffles itself by how fast each node answered is
// not a table anyone can read.
//
// Every node is attempted even when one fails, because a pass over a cluster
// that stops at the first problem hides the other four; the first error in
// node order is what comes back.
func eachNode[T any](nodes []*config.Node, parallel int, fn func(*config.Node) (T, error)) ([]T, error) {
	results := make([]T, len(nodes))
	errs := make([]error, len(nodes))

	if parallel < 1 {
		parallel = 1
	}

	var wg sync.WaitGroup

	sem := make(chan struct{}, parallel)

	for i, n := range nodes {
		wg.Add(1)

		go func(i int, n *config.Node) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			results[i], errs[i] = fn(n)
		}(i, n)
	}

	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return results, err
		}
	}

	return results, nil
}

// names lists node hostnames for a message about several of them at once.
func names(nodes []*config.Node) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Hostname)
	}

	return strings.Join(out, ", ")
}

// batches groups nodes that may be worked on at the same time.
//
// A control plane is always alone in its batch. Rebooting two at once is how a
// three-node control plane loses quorum, and --parallel is asked for to get
// through a hundred workers rather than to take chances with etcd. Config
// order is preserved: a run of workers batches up to the limit, and a control
// plane interrupts the run.
func batches(nodes []*config.Node, parallel int) [][]*config.Node {
	if parallel < 1 {
		parallel = 1
	}

	var (
		out     [][]*config.Node
		current []*config.Node
	)

	flush := func() {
		if len(current) > 0 {
			out = append(out, current)
			current = nil
		}
	}

	for _, n := range nodes {
		if n.IsControlPlane() {
			flush()
			out = append(out, []*config.Node{n})

			continue
		}

		current = append(current, n)

		if len(current) == parallel {
			flush()
		}
	}

	flush()

	return out
}
