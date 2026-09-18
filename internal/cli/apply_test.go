package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// fakeCluster returns a runner backed by a talosctl stand-in where the named
// addresses answer the maintenance service, one address answers with cluster
// PKI, and anything else answers nothing.
func fakeCluster(t *testing.T, running, maintenance []string) *talosctl.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	bin := filepath.Join(t.TempDir(), "talosctl")
	script := "#!/bin/sh\nnode=''\nwhile [ $# -gt 0 ]; do\n" +
		"  case \"$1\" in --nodes) node=$2 ;; --insecure) insecure=yes ;; esac\n  shift\ndone\n" +
		"for r in " + strings.Join(running, " ") + "; do\n" +
		"  [ \"$node\" = \"$r\" ] && { [ \"$insecure\" = yes ] && exit 1; exit 0; }\ndone\n" +
		"for m in " + strings.Join(maintenance, " ") + "; do\n" +
		"  [ \"$node\" = \"$m\" ] && { [ \"$insecure\" = yes ] && exit 0; exit 1; }\ndone\nexit 1\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	return talosctl.New(bin)
}

func targetsOf(addrs ...string) []*config.Node {
	out := make([]*config.Node, 0, len(addrs))

	for _, a := range addrs {
		out = append(out, &config.Node{Hostname: "n" + a[len(a)-1:], IPAddress: a})
	}

	return out
}

// --only-new is the adoption filter: what is already in the cluster must not
// be touched, and what talman cannot classify must not be guessed at.
func TestNewNodes(t *testing.T) {
	tal := fakeCluster(t, []string{"10.0.0.11"}, []string{"10.0.0.21", "10.0.0.22"})

	got, err := newNodes(tal, "/tmp/tc", targetsOf("10.0.0.11", "10.0.0.21", "10.0.0.22"), 4)
	if err != nil {
		t.Fatal(err)
	}

	var addrs []string
	for _, n := range got {
		addrs = append(addrs, n.IPAddress)
	}

	if want := "10.0.0.21,10.0.0.22"; strings.Join(addrs, ",") != want {
		t.Errorf("newNodes() kept %v, want %s", addrs, want)
	}
}

func TestNewNodesRefusesToGuessAboutADeadNode(t *testing.T) {
	tal := fakeCluster(t, []string{"10.0.0.11"}, nil)

	_, err := newNodes(tal, "/tmp/tc", targetsOf("10.0.0.11", "10.0.0.99"), 4)
	if err == nil {
		t.Fatal("expected an error for a node that answers neither API")
	}

	if !strings.Contains(err.Error(), "10.0.0.99") || !strings.Contains(err.Error(), "-n") {
		t.Errorf("error should name the node and the way out:\n%v", err)
	}
}

// The health gate checks the cluster the config describes, so it only means
// something once that cluster exists. A node that has not been adopted answers
// with a self-signed maintenance certificate, which the check reports as
// "certificate signed by unknown authority" -- a build-out step read as a
// broken cluster.
func TestNotInCluster(t *testing.T) {
	cfg := &config.Config{
		Nodes: []config.Node{
			{Hostname: "c01", IPAddress: "10.0.0.11", Role: config.RoleControlPlane},
			{Hostname: "c02", IPAddress: "10.0.0.12", Role: config.RoleControlPlane},
			{Hostname: "w01", IPAddress: "10.0.0.21", Role: config.RoleWorker},
		},
	}

	t.Run("half-built cluster names who is missing", func(t *testing.T) {
		tal := fakeCluster(t, []string{"10.0.0.11"}, []string{"10.0.0.12"})

		got := strings.Join(notInCluster(cfg, tal, "/tmp/tc", map[string]talosctl.Mode{}, 4), ", ")

		if !strings.Contains(got, "c02 is maintenance mode") || !strings.Contains(got, "w01 is unreachable") {
			t.Errorf("notInCluster() = %q", got)
		}
	})

	t.Run("fully built cluster gates", func(t *testing.T) {
		tal := fakeCluster(t, []string{"10.0.0.11", "10.0.0.12", "10.0.0.21"}, nil)

		if got := notInCluster(cfg, tal, "/tmp/tc", map[string]talosctl.Mode{}, 4); len(got) > 0 {
			t.Errorf("notInCluster() = %q, want none: every node answers with cluster PKI", got)
		}
	})

	t.Run("answers already in hand are not asked for again", func(t *testing.T) {
		// A runner that fails every call: if the cache is consulted, nothing
		// here needs it.
		tal := fakeCluster(t, nil, nil)

		modes := map[string]talosctl.Mode{
			"10.0.0.11": talosctl.ModeRunning,
			"10.0.0.12": talosctl.ModeRunning,
			"10.0.0.21": talosctl.ModeRunning,
		}

		if got := notInCluster(cfg, tal, "/tmp/tc", modes, 4); len(got) > 0 {
			t.Errorf("notInCluster() = %q, want none: the cached answers say the cluster is whole", got)
		}
	})
}

// Talos computes the diff on the node; "No changes." is the only part of that
// output talman reads. Anything else is a change, including output it does not
// recognise -- a CI job asking whether something changed is better told yes
// when talman cannot tell than no.
func TestDryRunChanged(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "no changes",
			out:  "NODE: 10.0.0.11\nDry run summary:\nConfig diff: No changes.\n",
			want: false,
		},
		{
			name: "a diff",
			out: "NODE: 10.0.0.11\nDry run summary:\nConfig diff:\n--- a\n+++ b\n" +
				"@@ -1 +1 @@\n-  hostname: old\n+  hostname: new\n",
			want: true,
		},
		{
			name: "a node with no config at all is a change",
			out:  "NODE: 10.0.0.21\nDry run summary:\nNode is running in maintenance mode and does not have a config yet.\n",
			want: true,
		},
		{
			name: "output talman does not recognise counts as changed",
			out:  "something talosctl started printing in a later release\n",
			want: true,
		},
		{
			name: "nothing at all counts as changed",
			out:  "",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dryRunChanged([]byte(tt.out)); got != tt.want {
				t.Errorf("dryRunChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
