package cli

import (
	"errors"
	"fmt"
	"io"
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

// maxParallel bounds --parallel.
//
// Not a limit anyone should meet: it is there so a typo asks a question rather
// than forking a thousand talosctl processes at a cluster.
const maxParallel = 64

// addParallelFlag registers --parallel with the given default, and the check
// that the number means something.
//
// Silently reading 0 or -1 as 1 would let a scripted typo change what a run
// does without saying so, which for apply and reset is the difference between
// one node at a time and something else entirely.
func addParallelFlag(cmd *cobra.Command, target *int, def int, help string) {
	cmd.Flags().IntVarP(target, "parallel", "p", def, help)

	previous := cmd.PreRunE

	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if *target < 1 || *target > maxParallel {
			return fmt.Errorf("--parallel %d is out of range: it must be between 1 and %d", *target, maxParallel)
		}

		if previous != nil {
			return previous(c, args)
		}

		return nil
	}
}

// eachNode runs fn over the nodes, at most parallel at a time, and returns the
// results in the order the nodes were given rather than the order they
// finished -- a table that reshuffles itself by how fast each node answered is
// not a table anyone can read.
//
// Every node is attempted even when one fails, because a pass over a cluster
// that stops at the first problem hides the other four -- and every failure
// comes back, joined in node order. Returning only the first would have let a
// batch where two nodes failed report one of them and leave the operator to
// discover the other from the cluster.
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

	return results, errors.Join(errs...)
}

// names lists node hostnames for a message about several of them at once.
func names(nodes []*config.Node) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Hostname)
	}

	return strings.Join(out, ", ")
}

// without drops the nodes already accounted for, keeping order.
//
// A batch fails as a unit but its nodes do not: three workers go together, one
// refuses the config, and the two that took it must not appear in the list of
// what still needs doing.
func without(nodes []*config.Node, done map[string]bool) []*config.Node {
	out := make([]*config.Node, 0, len(nodes))

	for _, n := range nodes {
		if !done[n.IPAddress] {
			out = append(out, n)
		}
	}

	return out
}

// batches groups nodes that may be worked on at the same time.
//
// A control plane is always alone in its batch. Rebooting two at once is how a
// three-node control plane loses quorum, and --parallel is asked for to get
// through a hundred workers rather than to take chances with etcd. Config
// order is preserved: a run of workers batches up to the limit, and a control
// plane interrupts the run.
func batches(nodes []*config.Node, parallel int) [][]*config.Node {
	return batchesFor(nodes, parallel, false)
}

// batchesFor is batches for a pass that may not be enacting anything: a dry
// run has no reboots to stagger, so control planes have nothing to be kept
// apart from.
func batchesFor(nodes []*config.Node, parallel int, inert bool) [][]*config.Node {
	if inert {
		return chunks(nodes, parallel)
	}

	return controlPlanesAlone(nodes, parallel)
}

func chunks(nodes []*config.Node, parallel int) [][]*config.Node {
	if parallel < 1 {
		parallel = 1
	}

	var out [][]*config.Node

	for start := 0; start < len(nodes); start += parallel {
		end := min(start+parallel, len(nodes))
		out = append(out, nodes[start:end])
	}

	return out
}

func controlPlanesAlone(nodes []*config.Node, parallel int) [][]*config.Node {
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

// inOrder prints what the nodes of a batch say in the order the batch lists
// them, however they finish.
//
// A batch runs its nodes at once, and each node's block used to be printed as
// it finished -- so a dry run over seven nodes read [7/7], [4/7], [6/7], in
// whatever order the cluster happened to answer. The node at the head of the
// order is printed as it speaks, so a slow one still shows progress; the rest
// are held until their turn.
type inOrder struct {
	mu    sync.Mutex
	w     io.Writer
	order []string
	next  int
	held  map[string]*strings.Builder
	done  map[string]bool
}

func newInOrder(w io.Writer, nodes []*config.Node) *inOrder {
	o := &inOrder{w: w, held: map[string]*strings.Builder{}, done: map[string]bool{}}

	for _, n := range nodes {
		o.order = append(o.order, n.IPAddress)
		o.held[n.IPAddress] = &strings.Builder{}
	}

	return o
}

// say prints text for a node, or holds it until the nodes before it are done.
func (o *inOrder) say(n *config.Node, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.next < len(o.order) && o.order[o.next] == n.IPAddress {
		fmt.Fprint(o.w, text)

		return
	}

	if b, ok := o.held[n.IPAddress]; ok {
		b.WriteString(text)

		return
	}

	// Not in this batch: nothing to keep it in order with.
	fmt.Fprint(o.w, text)
}

// finish marks a node done, and prints whoever it was holding up.
func (o *inOrder) finish(n *config.Node) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.done[n.IPAddress] = true

	for o.next < len(o.order) && o.done[o.order[o.next]] {
		o.next++

		if o.next < len(o.order) {
			head := o.held[o.order[o.next]]
			fmt.Fprint(o.w, head.String())
			head.Reset()
		}
	}
}

// nodeArgs spells out -n for each node, or nothing when the selection is the
// whole cluster anyway.
func nodeArgs(nodes []*config.Node, total int) string {
	if len(nodes) == total {
		return ""
	}

	var b strings.Builder

	for _, n := range nodes {
		b.WriteString(" -n " + n.Hostname)
	}

	return b.String()
}
