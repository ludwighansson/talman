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

// The node that failed is part of the remainder: its own work did not finish.
func TestResumeHint(t *testing.T) {
	got := resumeHint("reset", "reset", nodesOf("w2:worker", "c1:controlplane"), "--graceful=false")

	for _, want := range []string{
		"2 node(s) were not reset: w2, c1",
		"talman reset -n w2 -n c1 --graceful=false",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message does not contain %q:\n%s", want, got)
		}
	}

	applied := resumeHint("apply", "applied", nodesOf("w1:worker"))

	if !strings.Contains(applied, "1 node(s) were not applied: w1") ||
		!strings.Contains(applied, "continue with: talman apply -n w1") {
		t.Errorf("apply's remainder reads wrong:\n%s", applied)
	}

	if strings.Contains(applied, "--") {
		t.Errorf("no flags were given, so none should be suggested:\n%s", applied)
	}
}

// The prompt is where the operator decides, so it has to distinguish a node
// that comes back in maintenance mode from one that stops booting.
func TestConsequence(t *testing.T) {
	tests := []struct {
		name     string
		wipeDisk bool
		labels   []string
		reboot   bool
		want     string
	}{
		{
			name:   "default: partitions, then maintenance mode",
			labels: []string{"EPHEMERAL", "STATE"},
			reboot: true,
			want: "This wipes EPHEMERAL and STATE, destroying all data on them and their machine configs, " +
				"then reboots them into maintenance mode.",
		},
		{
			name:   "no reboot",
			labels: []string{"EPHEMERAL", "STATE"},
			reboot: false,
			want: "This wipes EPHEMERAL and STATE, destroying all data on them and their machine configs, " +
				"then shuts them down.",
		},
		{
			name:     "whole disk: nothing left to boot",
			wipeDisk: true,
			reboot:   true,
			want: "This wipes their system disks whole, Talos installation included, " +
				"then reboots them with nothing left to boot.",
		},
		{
			name:     "whole disk, powered off",
			wipeDisk: true,
			reboot:   false,
			want:     "This wipes their system disks whole, Talos installation included, then shuts them down.",
		},
		{
			// STATE holds the machine config: keep it and the node comes back
			// as itself, not in maintenance mode.
			name:   "narrowed to EPHEMERAL: the config survives",
			labels: []string{"EPHEMERAL"},
			reboot: true,
			want:   "This wipes EPHEMERAL, destroying all data on them, then reboots them, still holding their machine configs.",
		},
		{
			name:   "STATE only",
			labels: []string{"STATE"},
			reboot: true,
			want: "This wipes STATE, destroying all data on them and their machine configs, " +
				"then reboots them into maintenance mode.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := consequence(tt.wipeDisk, tt.labels, tt.reboot); got != tt.want {
				t.Errorf("consequence() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}
