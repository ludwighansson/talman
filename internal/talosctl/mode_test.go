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
			name:       "only the maintenance service answers",
			insecureOK: true,
			want:       ModeMaintenance,
			wantCalls:  2,
		},
		{
			name:      "neither answers",
			want:      ModeUnreachable,
			wantCalls: 2,
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
	if len(calls) != 2 {
		t.Fatalf("want a secure then an insecure probe, got:\n%s", strings.Join(calls, "\n"))
	}

	for _, call := range calls {
		if !strings.Contains(call, "--endpoints 10.0.0.11") {
			t.Errorf("probe is not pinned to the node: %s", call)
		}
	}

	// --insecure is a flag on `version`, not a global: talosctl rejects the
	// invocation when it comes before the subcommand.
	if !strings.HasSuffix(calls[1], "version --insecure") {
		t.Errorf("--insecure must follow the subcommand: %s", calls[1])
	}

	if strings.Contains(calls[1], "--talosconfig") {
		t.Errorf("the maintenance probe needs no talosconfig: %s", calls[1])
	}
}
