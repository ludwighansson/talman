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
