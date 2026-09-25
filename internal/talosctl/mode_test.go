package talosctl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTalosctl writes a talosctl stand-in that records its argv and exits with
// the given status per subcommand form, so Mode's decision can be tested
// without a cluster.
func fakeTalosctl(t *testing.T, secureOK, insecureOK bool) (*Runner, string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	bin := filepath.Join(dir, "talosctl")

	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"case \" $* \" in\n" +
		"  *\" --insecure \"*) exit " + exitFor(insecureOK) + " ;;\n" +
		"  *) exit " + exitFor(secureOK) + " ;;\n" +
		"esac\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	return &Runner{Bin: bin}, log
}

func exitFor(ok bool) string {
	if ok {
		return "0"
	}

	return "1"
}

func argv(t *testing.T, log string) []string {
	t.Helper()

	body, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(body)), "\n")
}

func TestMode(t *testing.T) {
	tests := []struct {
		name       string
		secureOK   bool
		insecureOK bool
		want       Mode
		wantCalls  int
	}{
		{
			name:      "cluster PKI answers: the node has joined",
			secureOK:  true,
			want:      ModeRunning,
			wantCalls: 1,
		},
		{
			// Pinned, then through the endpoints, then the maintenance
			// service: a node that answers none of those is unreachable.
			name:       "only the maintenance service answers",
			insecureOK: true,
			want:       ModeMaintenance,
			wantCalls:  3,
		},
		{
			name:      "neither answers",
			want:      ModeUnreachable,
			wantCalls: 3,
		},
		{
			// Belt and braces: a running node must not be reported as new
			// because the maintenance probe also happened to succeed.
			name:       "both answer: running wins, and the second call is never made",
			secureOK:   true,
			insecureOK: true,
			want:       ModeRunning,
			wantCalls:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, log := fakeTalosctl(t, tt.secureOK, tt.insecureOK)

			if got := r.Mode("/tmp/tc", "10.0.0.11"); got != tt.want {
				t.Errorf("Mode() = %v, want %v", got, tt.want)
			}

			if calls := argv(t, log); len(calls) != tt.wantCalls {
				t.Errorf("made %d call(s), want %d:\n%s", len(calls), tt.wantCalls, strings.Join(calls, "\n"))
			}
		})
	}
}

// Both probes have to address the machine itself. Without --endpoints talosctl
// proxies --nodes through the talosconfig endpoints, which are the control
// planes -- so the answer would describe the proxy, and a node in maintenance
// has no proxy path at all.
func TestModeProbesTheNodeDirectly(t *testing.T) {
	r, log := fakeTalosctl(t, false, true)

	if got := r.Mode("/tmp/tc", "10.0.0.11"); got != ModeMaintenance {
		t.Fatalf("Mode() = %v, want %v", got, ModeMaintenance)
	}

	calls := argv(t, log)
	if len(calls) != 3 {
		t.Fatalf("want pinned, then proxied, then insecure, got:\n%s", strings.Join(calls, "\n"))
	}

	if !strings.Contains(calls[0], "--endpoints 10.0.0.11") {
		t.Errorf("the first probe is not pinned to the node: %s", calls[0])
	}

	// The fallback is the route the apply itself uses: through the
	// talosconfig's endpoints, which is how a firewalled worker is reached.
	if strings.Contains(calls[1], "--endpoints") {
		t.Errorf("the second probe should go through the endpoints: %s", calls[1])
	}

	// --insecure is a flag on `version`, not a global: talosctl rejects the
	// invocation when it comes before the subcommand.
	if !strings.HasSuffix(calls[2], "version --insecure") {
		t.Errorf("--insecure must follow the subcommand: %s", calls[2])
	}

	if strings.Contains(calls[2], "--talosconfig") {
		t.Errorf("the maintenance probe needs no talosconfig: %s", calls[2])
	}
}

// A node reachable only through the control planes -- a worker whose API is
// not exposed outside the cluster network -- has to be found by the fallback,
// or an apply applies its config and then reports it as gone.
func TestAskNodeFallsBackToTheEndpoints(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "talosctl")
	log := filepath.Join(filepath.Dir(bin), "argv")

	// Fails whenever --endpoints names the node, succeeds otherwise: the
	// firewall that started this.
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\n" +
		"case \" $* \" in\n  *\" --endpoints 10.0.0.21 \"*) exit 1 ;;\n  *) exit 0 ;;\nesac\n"

	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	r := &Runner{Bin: bin}

	if got := r.Mode("/tmp/tc", "10.0.0.21"); got != ModeRunning {
		t.Errorf("Mode() = %v, want %v: the node answers through the endpoints", got, ModeRunning)
	}

	if !r.Reachable("/tmp/tc", "10.0.0.21") {
		t.Error("Reachable() = false: the node answers through the endpoints")
	}

	// The route that worked is remembered, so a wait polling every five
	// seconds does not pay the failing probe every time.
	before := len(argv(t, log))

	if !r.Reachable("/tmp/tc", "10.0.0.21") {
		t.Error("Reachable() = false on the second ask")
	}

	if added := len(argv(t, log)) - before; added != 1 {
		t.Errorf("the second ask made %d calls, want 1: the working route should be remembered", added)
	}
}

// "Is this node back?" has to be asked of the node. Routed through the
// talosconfig's endpoints it is answered by whichever control plane the client
// dialled, and on a cluster being built those are still in maintenance mode --
// so a node that never went away is reported as gone, and the apply waiting
// for it gives up after its timeout.
func TestReachableProbesTheNodeDirectly(t *testing.T) {
	r, log := fakeTalosctl(t, true, false)

	if !r.Reachable("/tmp/tc", "10.0.0.11") {
		t.Fatal("Reachable() = false, want true")
	}

	calls := argv(t, log)
	if len(calls) != 1 {
		t.Fatalf("want one probe, got:\n%s", strings.Join(calls, "\n"))
	}

	if !strings.Contains(calls[0], "--endpoints 10.0.0.11") {
		t.Errorf("probe is not pinned to the node: %s", calls[0])
	}
}

// The difference between "no etcd" and "etcd that is not healthy" is the
// difference between a cluster that was never bootstrapped and one that has
// lost quorum -- and the remedy for the first destroys the second.
func TestParseServiceRunning(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want EtcdState
	}{
		{
			name: "running and healthy: a cluster",
			out: "node: 10.0.0.11\nmetadata:\n    id: etcd\nspec:\n" +
				"    running: true\n    healthy: true\n    unknown: false\n",
			want: EtcdRunning,
		},
		{
			// A degraded cluster is still a cluster. Reading this as "not
			// bootstrapped" would have talman recommend bootstrapping over
			// the top of one that exists.
			name: "running but unhealthy: still a cluster",
			out:  "spec:\n    running: true\n    healthy: false\n",
			want: EtcdRunning,
		},
		{
			name: "not running: nothing has been bootstrapped here",
			out:  "spec:\n    running: false\n    healthy: false\n",
			want: EtcdStopped,
		},
		{
			// Running anywhere in the document wins: a nested block saying no
			// must not outvote the one saying yes.
			name: "one block says no, another says yes",
			out: "spec:\n    instances:\n      - running: false\n        healthy: false\n" +
				"    running: true\n    healthy: true\n",
			want: EtcdRunning,
		},
		{
			name: "a shape talman does not recognise establishes nothing",
			out:  "spec:\n    state: Running\n",
			want: EtcdUnknown,
		},
		{
			name: "nothing at all",
			out:  "",
			want: EtcdUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseServiceRunning([]byte(tt.out)); got != tt.want {
				t.Errorf("parseServiceRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestEtcdNotYetRegistered: a control plane that has just taken its config
// answers the Talos API before its etcd service exists, and talosctl reports
// that as NotFound. No etcd service is no etcd running -- the answer that
// lets apply see a cluster not yet bootstrapped -- not "cannot tell".
func TestEtcdNotYetRegistered(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "talosctl")

	script := "#!/bin/sh\necho 'rpc error: code = NotFound desc = resource ServiceStatuses.v1alpha1.talos.dev(runtime/etcd@undefined) doesn'\\''t exist' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := New(bin).Etcd("tc", "10.0.0.1"); got != EtcdStopped {
		t.Errorf("Etcd() = %v, want EtcdStopped", got)
	}

	// Anything else that fails is still not an answer.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'connection refused' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := New(bin).Etcd("tc", "10.0.0.1"); got != EtcdUnknown {
		t.Errorf("Etcd() = %v on a refused connection, want EtcdUnknown", got)
	}
}
