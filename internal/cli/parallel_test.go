package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
)

// Two control planes rebooting together is how a three-node cluster loses
// quorum, so --parallel buys speed across workers and never across them.
func TestBatches(t *testing.T) {
	tests := []struct {
		name     string
		nodes    []string
		parallel int
		want     string
	}{
		{
			name:     "serial keeps one node per batch",
			nodes:    []string{"w1:worker", "w2:worker", "c1:controlplane"},
			parallel: 1,
			want:     "w1 | w2 | c1",
		},
		{
			name:     "workers batch up to the limit",
			nodes:    []string{"w1:worker", "w2:worker", "w3:worker", "w4:worker", "w5:worker"},
			parallel: 2,
			want:     "w1, w2 | w3, w4 | w5",
		},
		{
			name:     "a control plane is always alone, and interrupts a run of workers",
			nodes:    []string{"w1:worker", "c1:controlplane", "w2:worker", "w3:worker"},
			parallel: 4,
			want:     "w1 | c1 | w2, w3",
		},
		{
			name:     "control planes never batch with each other",
			nodes:    []string{"c1:controlplane", "c2:controlplane", "c3:controlplane"},
			parallel: 8,
			want:     "c1 | c2 | c3",
		},
		{
			name:     "a nonsense limit still runs the nodes",
			nodes:    []string{"w1:worker", "w2:worker"},
			parallel: 0,
			want:     "w1 | w2",
		},
		{
			name:     "nothing selected",
			nodes:    nil,
			parallel: 4,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, batch := range batches(nodesOf(tt.nodes...), tt.parallel) {
				got = append(got, names(batch))
			}

			if strings.Join(got, " | ") != tt.want {
				t.Errorf("batches() = %q, want %q", strings.Join(got, " | "), tt.want)
			}
		})
	}
}

// Results follow the nodes, not the order they happened to finish in.
func TestEachNodeKeepsOrderAndReportsTheFirstFailure(t *testing.T) {
	nodes := nodesOf("a:worker", "b:worker", "c:worker", "d:worker")

	got, err := eachNode(nodes, 3, func(n *config.Node) (string, error) {
		return n.Hostname, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(got, ",") != "a,b,c,d" {
		t.Errorf("eachNode() = %v, want a,b,c,d", got)
	}

	_, err = eachNode(nodes, 3, func(n *config.Node) (string, error) {
		if n.Hostname == "b" || n.Hostname == "c" {
			return "", errors.New("node " + n.Hostname + " failed")
		}

		return n.Hostname, nil
	})

	if err == nil || !strings.Contains(err.Error(), "b") {
		t.Errorf("want the first failure in node order, got %v", err)
	}
}

// A batch fails as a unit but its nodes do not: what already succeeded must
// not turn up in the list of what still needs doing.
func TestWithout(t *testing.T) {
	nodes := nodesOf("w1:worker", "w2:worker", "w3:worker")

	got := names(without(nodes, map[string]bool{"10.0.0.1": true}))

	// nodesOf gives every node the same address, so all three drop out; the
	// point under test is that the filter keys on the address at all.
	if got != "" {
		t.Errorf("without() = %q, want empty", got)
	}

	if got := names(without(nodes, nil)); got != "w1, w2, w3" {
		t.Errorf("without(nil) = %q, want every node in order", got)
	}
}

// Inert passes have no reboots to stagger, so a control plane has nothing to
// be kept apart from and a fifty-node dry run need not take fifty turns.
func TestBatchesForInertPassesIgnoreRoles(t *testing.T) {
	nodes := nodesOf("c1:controlplane", "w1:worker", "c2:controlplane", "w2:worker")

	var got []string
	for _, batch := range batchesFor(nodes, 4, true) {
		got = append(got, names(batch))
	}

	if len(got) != 1 || got[0] != "c1, w1, c2, w2" {
		t.Errorf("batchesFor(inert) = %v, want one batch of everything", got)
	}

	// And the enacting path is unchanged.
	var enacting []string
	for _, batch := range batchesFor(nodes, 4, false) {
		enacting = append(enacting, names(batch))
	}

	if strings.Join(enacting, " | ") != "c1 | w1 | c2 | w2" {
		t.Errorf("batchesFor(enacting) = %v, want control planes alone", enacting)
	}
}

// A batch's nodes finish in whatever order the cluster answers, and their
// output is printed in the order the batch lists them regardless. The node at
// the head is printed as it speaks.
func TestInOrder(t *testing.T) {
	a, b, c := &config.Node{IPAddress: "a"}, &config.Node{IPAddress: "b"}, &config.Node{IPAddress: "c"}

	var w strings.Builder

	o := newInOrder(&w, []*config.Node{a, b, c})

	o.say(c, "c1\n")
	o.finish(c)
	o.say(b, "b1\n")

	if w.String() != "" {
		t.Fatalf("printed out of turn:\n%s", w.String())
	}

	o.say(a, "a1\n")

	if w.String() != "a1\n" {
		t.Fatalf("the head was not printed as it spoke:\n%s", w.String())
	}

	o.finish(a)
	o.say(b, "b2\n")
	o.finish(b)

	if got, want := w.String(), "a1\nb1\nb2\nc1\n"; got != want {
		t.Errorf("printed\n%s\nwant\n%s", got, want)
	}
}
