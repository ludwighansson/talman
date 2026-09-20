package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ludwighansson/talman/internal/config"
)

// `talosctl health` takes exactly one node to run from, and falls back to the
// talosconfig's default node list when given none -- which talman sets to
// every node in the cluster. Without --nodes the command died with
// `requires exactly one node (got 6)` before checking anything.
func TestHealthArgs(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{
			{Hostname: "c1", IPAddress: "10.0.0.11", Role: config.RoleControlPlane},
			{Hostname: "c2", IPAddress: "10.0.0.12", Role: config.RoleControlPlane},
			{Hostname: "w1", IPAddress: "10.0.0.21", Role: config.RoleWorker},
		},
	}

	got := strings.Join(healthArgs(cfg, "/tmp/tc", &cfg.Nodes[1], true, 0), " ")

	want := "--talosconfig /tmp/tc --endpoints 10.0.0.12 health --nodes 10.0.0.12 --server=true " +
		"--control-plane-nodes 10.0.0.11,10.0.0.12 --worker-nodes 10.0.0.21"

	if got != want {
		t.Errorf("healthArgs() =\n  %s\nwant\n  %s", got, want)
	}

	if client := strings.Join(healthArgs(cfg, "/tmp/tc", &cfg.Nodes[0], false, 0), " "); !strings.Contains(client, "--server=false") {
		t.Errorf("--server=false not forwarded: %s", client)
	}
}

// apply bounds the gate by its own --timeout, because talosctl otherwise
// waits twenty minutes to report that a gate will not pass.
func TestHealthArgsBoundsTheWait(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{{Hostname: "c1", IPAddress: "10.0.0.11", Role: config.RoleControlPlane}},
	}

	got := strings.Join(healthArgs(cfg, "/tmp/tc", &cfg.Nodes[0], false, 10*time.Minute), " ")

	if !strings.Contains(got, "--wait-timeout 10m0s") {
		t.Errorf("the gate is unbounded: %s", got)
	}

	if bare := strings.Join(healthArgs(cfg, "/tmp/tc", &cfg.Nodes[0], false, 0), " "); strings.Contains(bare, "--wait-timeout") {
		t.Errorf("`talman health` should keep talosctl's own patience: %s", bare)
	}
}

// A cluster with no workers must not get an empty --worker-nodes, which
// talosctl reads as a node named "".
func TestHealthArgsOmitsEmptyLists(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{{Hostname: "c1", IPAddress: "10.0.0.11", Role: config.RoleControlPlane}},
	}

	got := strings.Join(healthArgs(cfg, "/tmp/tc", &cfg.Nodes[0], true, 0), " ")

	if strings.Contains(got, "--worker-nodes") {
		t.Errorf("empty worker list was passed anyway: %s", got)
	}

	if !strings.Contains(got, "--control-plane-nodes 10.0.0.11") {
		t.Errorf("control plane list missing: %s", got)
	}
}
