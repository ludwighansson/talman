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

	got, err := newNodes(tal, "/tmp/tc", targetsOf("10.0.0.11", "10.0.0.21", "10.0.0.22"))
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

	_, err := newNodes(tal, "/tmp/tc", targetsOf("10.0.0.11", "10.0.0.99"))
	if err == nil {
		t.Fatal("expected an error for a node that answers neither API")
	}

	if !strings.Contains(err.Error(), "10.0.0.99") || !strings.Contains(err.Error(), "-n") {
		t.Errorf("error should name the node and the way out:\n%v", err)
	}
}
