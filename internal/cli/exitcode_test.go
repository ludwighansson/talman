package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubTalosctl is a talosctl that answers the calls apply and upgrade make,
// logs every invocation to $STUB_LOG, and takes its answers from the
// environment: STUB_DIFF is what a dry run reports, STUB_FAIL names a
// subcommand that fails, with STUB_CODE (default 1).
const stubTalosctl = `#!/bin/sh
echo "$*" >> "$STUB_LOG"

for a in "$@"; do
	case "$a" in
	"$STUB_FAIL") echo "stub: $a failed" >&2; exit "${STUB_CODE:-1}" ;;
	esac
done

case " $* " in
*" version --client "*) printf 'Client:\n\tTag:         v1.14.1\n' ;;
*" apply-config "*"--dry-run"*) printf 'Dry run summary:\nConfig diff:\n\n%s\n' "$STUB_DIFF" ;;
*" apply-config "*) echo "Applied configuration without a reboot" ;;
*" upgrade "*) echo "upgraded" ;;
*" version "*) echo "Server: Tag: v1.14.1" ;;
esac
`

// exitFixture is a one-node cluster directory with a rendered config and a
// talosconfig already in place, driven by the stub.
func exitFixture(t *testing.T) (dir, log string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the stub talosctl is a shell script")
	}

	dir = t.TempDir()
	log = filepath.Join(dir, "calls.log")
	stub := filepath.Join(dir, "talosctl")

	files := map[string]string{
		"talman.yaml": `apiVersion: talman.dev/v1
clusterName: stub
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.1
kubernetesVersion: v1.37.0
talosctl: ` + stub + `
nodes:
  - hostname: c1
    ipAddress: 10.0.0.1
    role: controlplane
`,
		"clusterconfig/c1.yaml":     "version: v1alpha1\n",
		"clusterconfig/talosconfig": "context: stub\n",
		"talosctl":                  stubTalosctl,
		"calls.log":                 "",
		"clusterconfig/.gitignore":  "*\n",
	}

	for rel, body := range files {
		path := filepath.Join(dir, rel)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("STUB_LOG", log)
	t.Setenv("STUB_DIFF", "No changes.")
	t.Setenv("STUB_FAIL", "")
	t.Setenv("STUB_CODE", "")
	t.Setenv("TALMAN_CONFIG", filepath.Join(dir, "talman.yaml"))
	t.Setenv("TALMAN_METRICS_FILE", "")
	t.Setenv("TALMAN_METRICS_URL", "")

	return dir, log
}

// TestExitCodes pins the contract CI jobs branch on: 0 nothing changed, 2
// something changed (only with --detailed-exit-code), 1 failure of any kind.
func TestExitCodes(t *testing.T) {
	apply := []string{"apply", "--no-render", "--redact-secrets=false", "--wait=false"}

	tests := []struct {
		name string
		args []string
		diff string
		fail string
		want int
	}{
		{"dry run, no drift", append(apply, "--dry-run", "--detailed-exit-code"), "No changes.", "", 0},
		{"dry run, drift", append(apply, "--dry-run", "--detailed-exit-code"), "+  hostname: new", "", 2},
		{"dry run, drift, no detailed exit code", append(apply, "--dry-run"), "+  hostname: new", "", 0},
		{"unreadable dry run counts as a change", append(apply, "--dry-run", "--detailed-exit-code"), "", "", 2},
		{"apply that changed", append(apply, "--detailed-exit-code"), "+  hostname: new", "", 2},
		{"apply that changed nothing", append(apply, "--detailed-exit-code"), "No changes.", "", 0},
		{"apply that failed", append(apply, "--detailed-exit-code"), "+  hostname: new", "apply-config", 1},
		{"upgrade that ran", []string{"upgrade", "--force", "--detailed-exit-code"}, "", "", 2},
		{"upgrade that failed", []string{"upgrade", "--force", "--detailed-exit-code"}, "", "upgrade", 1},
		{"upgrade dry run", []string{"upgrade", "--force", "--dry-run"}, "", "", 1},
		{"usage error", []string{"apply", "--no-such-flag"}, "", "", 1},
		{"unknown command", []string{"nodes"}, "", "", 1},
		{"unknown node", []string{"apply", "-n", "nope"}, "", "", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, log := exitFixture(t)

			t.Setenv("STUB_DIFF", tt.diff)
			t.Setenv("STUB_FAIL", tt.fail)

			if got := run(tt.args); got != tt.want {
				calls, _ := os.ReadFile(log)
				t.Errorf("talman %s exited %d, want %d\ntalosctl calls:\n%s",
					strings.Join(tt.args, " "), got, tt.want, calls)
			}
		})
	}
}

// TestDryRunSendsNothing holds apply --dry-run to its name: it asks each node
// what would change, and never sends a config for real.
func TestDryRunSendsNothing(t *testing.T) {
	_, log := exitFixture(t)

	t.Setenv("STUB_DIFF", "+  hostname: new")

	if got := run([]string{"apply", "--no-render", "--redact-secrets=false", "--dry-run"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(calls), "\n") {
		if strings.Contains(line, "apply-config") && !strings.Contains(line, "--dry-run") {
			t.Errorf("a dry run sent a config: %s", line)
		}
	}

	if !strings.Contains(string(calls), "apply-config") {
		t.Error("the dry run never asked the node what would change")
	}
}

// TestTalosctlPassthrough holds `talman talosctl` to its word: talosctl gets
// the talosconfig, the named nodes' addresses and every other argument as it
// was, talman parses none of them, and talosctl's exit status is talman's.
func TestTalosctlPassthrough(t *testing.T) {
	dir, log := exitFixture(t)

	if got := run([]string{"talosctl", "-n", "c1", "logs", "kubelet", "-f", "--tail", "5"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	calls, _ := os.ReadFile(log)
	want := "--talosconfig " + filepath.Join(dir, "clusterconfig", "talosconfig") +
		" --nodes 10.0.0.1 logs kubelet -f --tail 5"

	if !strings.Contains(string(calls), want) {
		t.Errorf("talosctl was called as\n%s\nwant a call ending in\n%s", calls, want)
	}

	t.Setenv("STUB_FAIL", "dmesg")
	t.Setenv("STUB_CODE", "3")

	if got := run([]string{"ctl", "--", "dmesg"}); got != 3 {
		t.Errorf("a talosctl exiting 3 gave exit %d, want its 3", got)
	}

	if got := run([]string{"talosctl", "-n", "nope", "dmesg"}); got != 1 {
		t.Errorf("an unknown node gave exit %d, want 1", got)
	}
}
