package cli

import (
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
)

func nodesOf(order ...string) []*config.Node {
	out := make([]*config.Node, 0, len(order))

	for _, spec := range order {
		name, role, _ := strings.Cut(spec, ":")

		out = append(out, &config.Node{
			Hostname:  name,
			IPAddress: "10.0.0.1",
			Role:      config.Role(role),
		})
	}

	return out
}

func names(nodes []*config.Node) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Hostname)
	}

	return strings.Join(out, ",")
}

// A control plane reset first takes out the endpoint every later node is
// reached through, and leaves nothing for a graceful reset to leave.
func TestResetOrder(t *testing.T) {
	tests := []struct {
		name    string
		targets []*config.Node
		want    string
	}{
		{
			name:    "config order: control planes first",
			targets: nodesOf("c1:controlplane", "c2:controlplane", "w1:worker", "w2:worker"),
			want:    "w1,w2,c1,c2",
		},
		{
			name:    "interleaved, and stable within each group",
			targets: nodesOf("c1:controlplane", "w1:worker", "c2:controlplane", "w2:worker"),
			want:    "w1,w2,c1,c2",
		},
		{
			name:    "workers only",
			targets: nodesOf("w1:worker", "w2:worker"),
			want:    "w1,w2",
		},
		{
			name:    "control planes only",
			targets: nodesOf("c1:controlplane", "c2:controlplane"),
			want:    "c1,c2",
		},
		{
			name:    "nothing selected",
			targets: nil,
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := names(resetOrder(tt.targets)); got != tt.want {
				t.Errorf("resetOrder() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The node that failed is part of the remainder: its own reset did not finish.
func TestUnresetNamesTheRemainderAndHowToResume(t *testing.T) {
	got := unreset(nodesOf("w2:worker", "c1:controlplane"), false)

	for _, want := range []string{
		"2 node(s) were not reset: w2, c1",
		"talman reset -n w2 -n c1 --graceful=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message does not contain %q:\n%s", want, got)
		}
	}

	if strings.Contains(unreset(nodesOf("w1:worker"), true), "--graceful") {
		t.Error("a graceful pass should not be resumed with --graceful=false")
	}
}
