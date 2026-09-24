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
// subcommand that fails, with STUB_CODE (default 1), and STUB_STALE an address
// whose node runs an older Talos than the config wants.
const stubTalosctl = `#!/bin/sh
echo "$*" >> "$STUB_LOG"

for a in "$@"; do
	case "$a" in
	"$STUB_FAIL") echo "stub: $a failed" >&2; exit "${STUB_CODE:-1}" ;;
	esac
done

output=
prev=
for a in "$@"; do
	[ "$prev" = "--output" ] && output=$a
	prev=$a
done

case " $* " in
*" version --client "*) printf 'Client:\n\tTag:         v1.14.1\n' ;;
*" apply-config "*"--dry-run"*) printf 'Dry run summary:\nConfig diff:\n\n%s\n' "$STUB_DIFF" ;;
*" apply-config "*) echo "Applied configuration without a reboot" ;;
*" upgrade "*) echo "upgraded" ;;
*" reboot "*) echo "rebooted" ;;
*" rotate-ca "*"--dry-run=false"*) echo "context: rotated" > "$output" ;;
*" rotate-ca "*) echo "would rotate" ;;
*" read /system/state/config.yaml"*) echo "machine: {ca: new}" ;;
*" gen secrets "*) echo "bundle: rotated" ;;
*" etcd snapshot "*) eval "out=\${$#}"; umask 022; echo "snapshot" > "$out" ;;
*" version "*)
	running=v1.14.1
	case " $* " in *" $STUB_STALE "*) running=v1.13.0 ;; esac
	printf 'Client:\n\tTag: v1.14.1\nServer:\n\tTag: %s\n' "$running" ;;
esac
`

// exitFixture is a one-node cluster directory with a rendered config and a
// talosconfig already in place, driven by the stub.
func exitFixture(t *testing.T) (dir, log string) {
	t.Helper()

	return exitFixtureWith(t, "")
}

// exitFixtureWith is exitFixture with more nodes after the control plane.
func exitFixtureWith(t *testing.T, moreNodes string) (dir, log string) {
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
` + moreNodes,
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
	t.Setenv("STUB_STALE", "")
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

// TestReboot checks the rolling reboot's shape: one talosctl reboot per node,
// in config order, with the mode and the wait it was given; and a failure
// stops the roll-out and exits 1.
func TestReboot(t *testing.T) {
	_, log := exitFixture(t)

	if got := run([]string{"reboot", "--mode", "powercycle"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "reboot --nodes 10.0.0.1 --mode powercycle --wait=true --timeout 30m0s") {
		t.Errorf("no reboot call as expected in:\n%s", calls)
	}

	if got := run([]string{"reboot", "--mode", "gently"}); got != 1 {
		t.Errorf("an unknown mode gave exit %d, want 1", got)
	}

	t.Setenv("STUB_FAIL", "reboot")

	if got := run([]string{"reboot"}); got != 1 {
		t.Errorf("a failed reboot gave exit %d, want 1", got)
	}
}

// TestEtcdSnapshot checks the snapshot lands in the output directory, 0600
// whatever talosctl's umask made it, and that upgrade --snapshot takes one
// before upgrading.
func TestEtcdSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}

	dir, log := exitFixture(t)

	if got := run([]string{"etcd", "snapshot"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	snaps, _ := filepath.Glob(filepath.Join(dir, "clusterconfig", "etcd-stub-*.db"))
	if len(snaps) != 1 {
		t.Fatalf("snapshots in the output directory: %v", snaps)
	}

	info, err := os.Stat(snaps[0])
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot is %o, want 600", perm)
	}

	named := filepath.Join(dir, "before.db")

	if got := run([]string{"etcd", "snapshot", named}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	if got := run([]string{"etcd", "snapshot", named}); got != 1 {
		t.Errorf("overwriting a snapshot gave exit %d, want 1", got)
	}

	_ = os.WriteFile(log, nil, 0o644)

	if got := run([]string{"upgrade", "--force", "--snapshot"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	calls, _ := os.ReadFile(log)
	snap := strings.Index(string(calls), "etcd snapshot")
	upgrade := strings.Index(string(calls), " upgrade ")

	if snap < 0 || upgrade < 0 || snap > upgrade {
		t.Errorf("upgrade --snapshot did not snapshot before upgrading:\n%s", calls)
	}
}

// TestRotateCA follows a rotation through: talosctl rotates across every
// node, the new bundle is extracted from a control plane through the new
// talosconfig, it replaces the old bundle with the old one kept beside it, and
// the talosconfig is swapped for the rotated one. A dry run touches nothing.
func TestRotateCA(t *testing.T) {
	dir, log := exitFixture(t)

	secrets := filepath.Join(dir, "secrets.sops.yaml")
	if err := os.WriteFile(secrets, []byte("bundle: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tc := filepath.Join(dir, "clusterconfig", "talosconfig")

	if got := run([]string{"rotate-ca", "--dry-run"}); got != 0 {
		t.Fatalf("dry run: exit %d", got)
	}

	if b, _ := os.ReadFile(secrets); string(b) != "bundle: old\n" {
		t.Errorf("a dry run changed the bundle to %q", b)
	}

	if got := run([]string{"rotate-ca", "-y", "--kubernetes=false"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Fatalf("exit %d\n%s", got, calls)
	}

	calls, _ := os.ReadFile(log)

	for _, want := range []string{
		"rotate-ca --control-plane-nodes 10.0.0.1 --talos=true --kubernetes=false --dry-run=false",
		"--talosconfig " + tc + ".rotated --nodes 10.0.0.1 read /system/state/config.yaml",
		"gen secrets --output-file - --talos-version v1.14.1 --from-controlplane-config",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("no call containing %q in:\n%s", want, calls)
		}
	}

	if b, _ := os.ReadFile(secrets); string(b) != "bundle: rotated\n" {
		t.Errorf("bundle = %q, want the one extracted after the rotation", b)
	}

	backups, _ := filepath.Glob(secrets + ".pre-rotate-*")
	if len(backups) != 1 {
		t.Fatalf("old bundle backups: %v", backups)
	} else if b, _ := os.ReadFile(backups[0]); string(b) != "bundle: old\n" {
		t.Errorf("backup = %q", b)
	}

	if b, _ := os.ReadFile(tc); string(b) != "context: rotated\n" {
		t.Errorf("talosconfig = %q, want the rotated one", b)
	}

	if got := run([]string{"rotate-ca", "--talos=false", "--kubernetes=false"}); got != 1 {
		t.Errorf("rotating nothing gave exit %d, want 1", got)
	}
}

// TestUpgradeDoesOnlyWhatItMust: an upgrade that finds nothing to do takes no
// etcd snapshot, and the health gate runs only after a batch that upgraded
// something -- not after batches of nodes it skipped.
func TestUpgradeDoesOnlyWhatItMust(t *testing.T) {
	workers := `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
  - hostname: w2
    ipAddress: 10.0.0.3
    role: worker
`

	_, log := exitFixtureWith(t, workers)

	if got := run([]string{"upgrade", "--snapshot", "--detailed-exit-code"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Fatalf("an up-to-date cluster gave exit %d\n%s", got, calls)
	}

	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "etcd snapshot") {
		t.Error("upgrade --snapshot took a snapshot with nothing to upgrade")
	}

	_ = os.WriteFile(log, nil, 0o644)

	t.Setenv("STUB_STALE", "10.0.0.3")

	if got := run([]string{"upgrade", "--health", "--snapshot", "--detailed-exit-code"}); got != 2 {
		calls, _ := os.ReadFile(log)
		t.Fatalf("exit %d, want 2\n%s", got, calls)
	}

	calls, _ = os.ReadFile(log)

	if n := strings.Count(string(calls), " health "); n != 0 {
		t.Errorf("the health gate ran %d time(s) after batches that upgraded nothing:\n%s", n, calls)
	}

	snap := strings.Index(string(calls), "etcd snapshot")
	upgrade := strings.Index(string(calls), " upgrade --nodes 10.0.0.3")

	if snap < 0 || upgrade < 0 || snap > upgrade {
		t.Errorf("no snapshot before the one upgrade that ran:\n%s", calls)
	}
}
