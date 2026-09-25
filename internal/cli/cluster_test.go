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

// fakeEtcd returns a runner whose talosctl answers `get services etcd` per
// node: "running", "stopped", or "silent" for a node that does not answer.
func fakeEtcd(t *testing.T, answers map[string]string) *talosctl.Runner {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	var cases strings.Builder

	for addr, answer := range answers {
		body := `printf 'spec:\n    running: true\n    healthy: true\n'`

		switch answer {
		case "stopped":
			body = `printf 'spec:\n    running: false\n    healthy: false\n'`
		case "unhealthy":
			body = `printf 'spec:\n    running: true\n    healthy: false\n'`
		case "silent":
			body = "exit 1"
		}

		cases.WriteString("    *\" --nodes " + addr + " \"*) " + body + " ;;\n")
	}

	bin := filepath.Join(t.TempDir(), "talosctl")
	script := "#!/bin/sh\ncase \" $* \" in\n" +
		"  *\" get services etcd \"*)\n    case \" $* \" in\n" + cases.String() +
		"    *) exit 1 ;;\n    esac ;;\n  *) exit 1 ;;\nesac\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	return talosctl.New(bin)
}

func clusterOf(nodes ...string) *config.Config {
	cfg := &config.Config{}

	for _, spec := range nodes {
		addr, role, _ := strings.Cut(spec, ":")
		cfg.Nodes = append(cfg.Nodes, config.Node{
			Hostname:  addr,
			IPAddress: addr,
			Role:      config.Role(role),
		})
	}

	return cfg
}

// Whether there is a cluster decides whether talman recommends bootstrapping
// one, and whether apply keeps the gates that stop a bad config from reaching
// a whole control plane. Getting it wrong in either direction is expensive:
// bootstrapping over a live cluster destroys it, and dropping the gates on a
// silent probe removes the protection when least is known.
func TestAskCluster(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []string
		answers map[string]string
		want    clusterState
	}{
		{
			name:    "etcd running: a cluster",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.21:worker"},
			answers: map[string]string{"10.0.0.11": "running"},
			want:    clusterUp,
		},
		{
			// The one that would have destroyed a cluster: quorum lost reads
			// as running and unhealthy, and the remedy is anything but
			// bootstrapping over the top of it.
			name:    "etcd running but unhealthy: still a cluster",
			nodes:   []string{"10.0.0.11:controlplane"},
			answers: map[string]string{"10.0.0.11": "unhealthy"},
			want:    clusterUp,
		},
		{
			name:    "etcd stopped everywhere: nothing bootstrapped",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.12:controlplane"},
			answers: map[string]string{"10.0.0.11": "stopped", "10.0.0.12": "stopped"},
			want:    clusterAbsent,
		},
		{
			// Silence establishes nothing. Reading it as "no cluster" is what
			// let a probe that reached nobody switch off apply's gates.
			name:    "no control plane answers: nothing established",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.12:controlplane"},
			answers: map[string]string{"10.0.0.11": "silent", "10.0.0.12": "silent"},
			want:    clusterUnknown,
		},
		{
			name:    "one silent, one stopped: the answer that came back decides",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.12:controlplane"},
			answers: map[string]string{"10.0.0.11": "silent", "10.0.0.12": "stopped"},
			want:    clusterAbsent,
		},
		{
			name:    "one stopped, one running: a cluster exists",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.12:controlplane"},
			answers: map[string]string{"10.0.0.11": "stopped", "10.0.0.12": "running"},
			want:    clusterUp,
		},
		{
			// `talman status -n worker-01` used to ask only the nodes it was
			// given, find no control plane, and announce that the cluster was
			// not bootstrapped.
			name:    "workers are never asked",
			nodes:   []string{"10.0.0.11:controlplane", "10.0.0.21:worker"},
			answers: map[string]string{"10.0.0.11": "silent", "10.0.0.21": "running"},
			want:    clusterUnknown,
		},
		{
			name:  "a config with no control planes establishes nothing",
			nodes: []string{"10.0.0.21:worker"},
			want:  clusterUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := askCluster(clusterOf(tt.nodes...), fakeEtcd(t, tt.answers), "/tmp/tc"); got != tt.want {
				t.Errorf("askCluster() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClusterCheckRemembersOnlyAnswers: whether a cluster exists is asked
// again until it is known. The first ask in a fresh build comes moments after
// the first control plane took its config, before its etcd service exists;
// remembering "unknown" from then made every later node wait for a cluster
// that could not exist yet.
func TestClusterCheckRemembersOnlyAnswers(t *testing.T) {
	answers := []clusterState{clusterUnknown, clusterAbsent, clusterUp}
	asked := 0

	noClusterYet := clusterCheck(func() clusterState {
		a := answers[asked]
		asked++

		return a
	})

	if noClusterYet() {
		t.Error("an unknown answer read as no cluster")
	}

	if !noClusterYet() {
		t.Error("asking again after unknown did not find the cluster absent")
	}

	if !noClusterYet() || asked != 2 {
		t.Errorf("a known answer was asked again (%d asks)", asked)
	}
}
