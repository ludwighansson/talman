package cli

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ludwighansson/talman/internal/sopsx"
)

// stubTalosctl is a talosctl that answers the calls apply and upgrade make,
// logs every invocation to $STUB_LOG, and takes its answers from the
// environment: STUB_DIFF is what a dry run reports, STUB_FAIL names a
// subcommand that fails, with STUB_CODE (default 1), and STUB_STALE an address
// whose node runs an older Talos than the config wants.
const stubTalosctl = `#!/bin/sh
echo "$*" >> "$STUB_LOG"

# STUB_DEAD_ENDPOINTS: the talosconfig's endpoints are down, so only a call
# pinned to a node with --endpoints gets through.
# STUB_DEAD_DIRECT: nodes are only reachable through the talosconfig's
# endpoints, so a call pinned to one with --endpoints fails.
if [ -n "$STUB_DEAD_DIRECT" ]; then
	case " $* " in
	*" --endpoints "*) echo "stub: no route to the node" >&2; exit 1 ;;
	esac
fi

if [ -n "$STUB_DEAD_ENDPOINTS" ]; then
	case " $* " in
	*" --endpoints "*|*" version --client "*) ;;
	*" etcd snapshot "*|*" read "*) echo "stub: endpoints unreachable" >&2; exit 1 ;;
	esac
fi

for a in "$@"; do
	case "$a" in
	"$STUB_KILL") kill -TERM $$ ;;
	"$STUB_FAIL") echo "stub: $a failed" >&2; exit "${STUB_CODE:-1}" ;;
	esac
done

# rotate-ca, as talosctl's, runs through exactly one node: none falls back to
# the talosconfig's every node, and more than one is refused.
case " $* " in
*" rotate-ca "*)
	nodes=
	prev=
	for a in "$@"; do
		[ "$prev" = "--nodes" ] && nodes=$a
		[ "$a" = "rotate-ca" ] && break
		prev=$a
	done
	case "$nodes" in
	""|*,*) echo 'command "rotate-ca" requires exactly one node' >&2; exit 1 ;;
	esac ;;
esac

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
*" get services etcd "*) printf 'spec:\n    running: %s\n    healthy: %s\n' "${STUB_ETCD:-true}" "${STUB_ETCD:-true}" ;;
*" reboot "*) echo "rebooted" ;;
*" rotate-ca "*"--dry-run=false"*)
	echo "context: rotated" > "$output"
	if [ -n "$STUB_ROTATE_PARTWAY" ]; then echo "stub: kubernetes rotation failed" >&2; exit 1; fi
	;;
*" rotate-ca "*) echo "would rotate" ;;
*" read /system/state/config.yaml"*) echo "machine: {ca: new}" ;;
*" gen secrets "*) echo "bundle: rotated" ;;
*" etcd snapshot "*)
	eval "out=\${$#}"
	# Another run landing a snapshot on the same name mid-stream.
	[ -n "$STUB_RACE" ] && echo "someone else's" > "$STUB_RACE"
	# What another user on the host would see while the stream runs.
	ls -ld "$(dirname "$out")" | cut -c1-10 >> "$STUB_LOG.dirmode"
	umask 022
	echo "snapshot" > "$out" ;;
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
	t.Setenv("STUB_KILL", "")
	t.Setenv("STUB_DEAD_ENDPOINTS", "")
	t.Setenv("STUB_DEAD_DIRECT", "")
	t.Setenv("STUB_RACE", "")
	t.Setenv("STUB_ROTATE_PARTWAY", "")
	t.Setenv("STUB_ETCD", "")
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

	// talosctl's own flags reach it wherever they are, before its
	// subcommand included, and -c still reaches talman.
	_ = os.WriteFile(log, nil, 0o644)

	if got := run([]string{"ctl", "-c", filepath.Join(dir, "talman.yaml"), "-n", "c1",
		"--endpoints", "10.9.9.9", "-e", "10.9.9.8", "version"}); got != 0 {
		t.Fatalf("talosctl flags before its subcommand: exit %d", got)
	}

	calls, _ = os.ReadFile(log)
	if !strings.Contains(string(calls), "--nodes 10.0.0.1 --endpoints 10.9.9.9 -e 10.9.9.8 version") {
		t.Errorf("talosctl's flags did not reach it as given:\n%s", calls)
	}

	t.Setenv("STUB_FAIL", "dmesg")
	t.Setenv("STUB_CODE", "3")

	if got := run([]string{"ctl", "--", "dmesg"}); got != 3 {
		t.Errorf("a talosctl exiting 3 gave exit %d, want its 3", got)
	}

	// Killed by a signal, talosctl has no exit status of its own; talman
	// reports it the way a shell would, 128 + the signal.
	t.Setenv("STUB_KILL", "logs")

	if got := run([]string{"ctl", "logs", "kubelet"}); got != 128+15 {
		t.Errorf("a talosctl killed by SIGTERM gave exit %d, want 143", got)
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

	// While talosctl wrote it, it was in a directory no one else can enter.
	if modes, _ := os.ReadFile(log + ".dirmode"); !strings.HasPrefix(string(modes), "drwx------") {
		t.Errorf("talosctl streamed the snapshot into a directory with mode %q", modes)
	}

	if leftovers, _ := filepath.Glob(filepath.Join(dir, "clusterconfig", ".talman-snapshot-*")); len(leftovers) > 0 {
		t.Errorf("staging directories left behind: %v", leftovers)
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
		"--talosconfig " + tc + ".rotated --endpoints 10.0.0.1 --nodes 10.0.0.1 read /system/state/config.yaml",
		"gen secrets --output-file - --talos-version v1.14.1 --from-controlplane-config",
		// The rotated talosconfig gets render's endpoints and nodes, not the
		// one pinned control plane talosctl wrote it with.
		"--talosconfig " + tc + ".rotated config endpoint 10.0.0.1",
		"--talosconfig " + tc + ".rotated config node 10.0.0.1",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("no call containing %q in:\n%s", want, calls)
		}
	}

	if b, _ := os.ReadFile(secrets); string(b) != "bundle: rotated\n" {
		t.Errorf("bundle = %q, want the one extracted after the rotation", b)
	}

	backups, _ := filepath.Glob(filepath.Join(dir, "clusterconfig", "secrets-pre-rotate-*.yaml"))
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

// TestSnapshotFollowsTheRouteThatAnswered: the control plane was found by
// asking it directly, so the snapshot and rotate-ca's read of its config go
// the same way rather than through talosconfig endpoints that may be down.
func TestSnapshotFollowsTheRouteThatAnswered(t *testing.T) {
	dir, log := exitFixture(t)

	t.Setenv("STUB_DEAD_ENDPOINTS", "1")

	if got := run([]string{"etcd", "snapshot"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Errorf("etcd snapshot with dead endpoints: exit %d\n%s", got, calls)
	}

	if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), []byte("bundle: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := run([]string{"rotate-ca", "-y", "--kubernetes=false"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Errorf("rotate-ca with dead endpoints: exit %d\n%s", got, calls)
	}
}

// TestRotateCAPartway: a rotation talosctl abandoned after the Talos CA leaves
// the bundle untouched and the talosconfig in place, and talman's error has to
// say where the working talosconfig is and how to finish.
func TestRotateCAPartway(t *testing.T) {
	dir, _ := exitFixture(t)

	secrets := filepath.Join(dir, "secrets.sops.yaml")
	if err := os.WriteFile(secrets, []byte("bundle: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("STUB_ROTATE_PARTWAY", "1")

	stderr := captureStderr(t, func() {
		if got := run([]string{"rotate-ca", "-y"}); got != 1 {
			t.Errorf("exit %d, want 1", got)
		}
	})

	for _, want := range []string{
		"did not finish",
		"talosconfig.rotated",
		"secrets generate --force --from-controlplane-config",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the error does not mention %q:\n%s", want, stderr)
		}
	}

	if b, _ := os.ReadFile(secrets); string(b) != "bundle: old\n" {
		t.Errorf("an unfinished rotation replaced the bundle with %q", b)
	}
}

// captureStderr runs fn with os.Stderr redirected, and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	saved := os.Stderr
	os.Stderr = w

	done := make(chan string)

	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	os.Stderr = saved
	_ = w.Close()

	return <-done
}

// TestRotateCALeavesTheRotatedTalosconfigAlone: after a rotation that stopped
// partway, talosconfig.rotated may be the only way into the cluster, so no
// later run -- a dry run included -- may delete it.
func TestRotateCALeavesTheRotatedTalosconfigAlone(t *testing.T) {
	dir, log := exitFixture(t)

	if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), []byte("bundle: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rotated := filepath.Join(dir, "clusterconfig", "talosconfig.rotated")
	if err := os.WriteFile(rotated, []byte("context: the only one that works\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"rotate-ca", "--dry-run"}, {"rotate-ca", "-y", "--talos=false"}} {
		if got := run(args); got != 1 {
			t.Errorf("%v with a rotated talosconfig left over: exit %d, want 1", args, got)
		}

		if b, _ := os.ReadFile(rotated); string(b) != "context: the only one that works\n" {
			t.Fatalf("%v removed or changed the rotated talosconfig", args)
		}
	}

	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "rotate-ca") {
		t.Errorf("talosctl rotate-ca ran anyway:\n%s", calls)
	}
}

// TestRotateCAChecksEncryptionFirst: an encrypted bundle that could not be
// re-encrypted is found out before the cluster's CAs change, not after.
func TestRotateCAChecksEncryptionFirst(t *testing.T) {
	dir, log := exitFixture(t)

	encrypted := "bundle: ENC[AES256_GCM,data:x]\nsops:\n  version: 3.13.3\n"
	if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), []byte(encrypted), 0o600); err != nil {
		t.Fatal(err)
	}

	saved := sopsx.Bin
	sopsx.Bin = filepath.Join(dir, "no-sops-here")

	t.Cleanup(func() { sopsx.Bin = saved })

	if got := run([]string{"rotate-ca", "-y"}); got != 1 {
		t.Errorf("exit %d, want 1", got)
	}

	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "rotate-ca") {
		t.Errorf("the CAs were rotated before the bundle's encryption was known to work:\n%s", calls)
	}
}

// TestNoWaitKeepsControlPlanesApart: without --wait each command returns
// before its node is back, so more than one control plane selected would go
// down together. Refused, for every command that reboots.
func TestNoWaitKeepsControlPlanesApart(t *testing.T) {
	cps := `  - hostname: c2
    ipAddress: 10.0.0.2
    role: controlplane
`

	_, log := exitFixtureWith(t, cps)

	for _, args := range [][]string{
		{"upgrade", "--force", "--wait=false"},
		{"reboot", "--wait=false"},
		{"apply", "--no-render", "--redact-secrets=false", "--wait=false"},
	} {
		if got := run(args); got != 1 {
			t.Errorf("%v with two control planes: exit %d, want 1", args, got)
		}
	}

	calls, _ := os.ReadFile(log)
	for _, verb := range []string{" upgrade --nodes", " reboot --nodes", " apply-config "} {
		if strings.Contains(string(calls), verb) {
			t.Errorf("%q ran anyway:\n%s", verb, calls)
		}
	}

	if got := run([]string{"reboot", "--wait=false", "-n", "c1"}); got != 0 {
		t.Errorf("one control plane without waiting: exit %d, want 0", got)
	}
}

// TestStatusJSONBootstrapped: the field says true or false whenever the
// control planes answered, so a script can tell a bootstrapped cluster from
// one nobody asked about.
func TestStatusJSONBootstrapped(t *testing.T) {
	exitFixture(t)

	for etcd, want := range map[string]string{"true": `"bootstrapped": true`, "false": `"bootstrapped": false`} {
		t.Setenv("STUB_ETCD", etcd)

		out := captureStdout(t, func() {
			if got := run([]string{"status", "-o", "json"}); got != 0 {
				t.Errorf("exit %d", got)
			}
		})

		if !strings.Contains(out, want) {
			t.Errorf("etcd running=%s: no %s in\n%s", etcd, want, out)
		}
	}

	out := captureStdout(t, func() { run([]string{"status", "-o", "json", "--offline"}) })
	if strings.Contains(out, "bootstrapped") {
		t.Errorf("an offline status claims to know:\n%s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	saved := os.Stdout
	os.Stdout = w

	done := make(chan string)

	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	fn()

	os.Stdout = saved
	_ = w.Close()

	return <-done
}

// TestApplyGatesBetweenBatches: on the shared roll-out loop, apply still gates
// between batches and not after the last.
func TestApplyGatesBetweenBatches(t *testing.T) {
	dir, log := exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
`)

	if err := os.WriteFile(filepath.Join(dir, "clusterconfig", "w1.yaml"), []byte("version: v1alpha1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A config that changes something, so there is something to gate on.
	t.Setenv("STUB_DIFF", "+  hostname: new")

	if got := run([]string{"apply", "--no-render", "--redact-secrets=false", "--wait=false", "--health"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Fatalf("exit %d\n%s", got, calls)
	}

	calls, _ := os.ReadFile(log)
	if n := strings.Count(string(calls), " health "); n != 1 {
		t.Errorf("health gate ran %d time(s), want once, between c1 and w1:\n%s", n, calls)
	}
}

const waveNodes = `  - hostname: g1
    ipAddress: 10.0.0.3
    role: worker
    groups: [green]
  - hostname: b1
    ipAddress: 10.0.0.2
    role: worker
    groups: [blue]
  - hostname: p1
    ipAddress: 10.0.0.4
    role: worker
    groups: [pink]
  - hostname: x1
    ipAddress: 10.0.0.5
    role: worker
`

// rebooted lists the nodes talosctl was asked to reboot, in order.
func rebooted(t *testing.T, log string) string {
	t.Helper()

	calls, _ := os.ReadFile(log)

	var out []string

	for line := range strings.SplitSeq(string(calls), "\n") {
		if _, rest, ok := strings.Cut(line, " reboot --nodes "); ok {
			ip, _, _ := strings.Cut(rest, " ")
			out = append(out, ip)
		}
	}

	return strings.Join(out, " ")
}

// TestRolloutWaves: the config's waves order a roll-out, and --wave, --from,
// --until and -g select from them without reordering.
func TestRolloutWaves(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes+`rollout:
  soak: 10ms
  waves:
    - controlplane
    - blue
    - [green, pink]
`)

	for _, tt := range []struct {
		args []string
		want string
		hint string
	}{
		// c1, then blue, then green and pink in config order, then the rest.
		{[]string{"reboot"}, "10.0.0.1 10.0.0.2 10.0.0.3 10.0.0.4 10.0.0.5", ""},
		{[]string{"reboot", "--until", "blue"}, "10.0.0.1 10.0.0.2", "--from green"},
		{[]string{"reboot", "--from", "pink"}, "10.0.0.3 10.0.0.4 10.0.0.5", ""},
		{[]string{"reboot", "--wave", "green"}, "10.0.0.3 10.0.0.4", ""},
		{[]string{"reboot", "--wave", "rest"}, "10.0.0.5", ""},
		{[]string{"reboot", "-g", "pink", "-g", "blue"}, "10.0.0.2 10.0.0.4", ""},
		{[]string{"reboot", "-g", "worker", "--until", "blue"}, "10.0.0.2", "--from green"},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			_ = os.WriteFile(log, nil, 0o644)

			var got int

			stderr := captureStderr(t, func() { got = run(tt.args) })

			if got != 0 {
				t.Fatalf("exit %d\n%s", got, stderr)
			}

			if order := rebooted(t, log); order != tt.want {
				t.Errorf("rebooted %q, want %q\n%s", order, tt.want, stderr)
			}

			if tt.hint != "" && !strings.Contains(stderr, tt.hint) {
				t.Errorf("no %q in the hint:\n%s", tt.hint, stderr)
			}
		})
	}

	stderr := captureStderr(t, func() { run([]string{"reboot"}) })
	if !strings.Contains(stderr, "== wave 2/4: blue, 1 node(s)") || !strings.Contains(stderr, "soaking 10ms") {
		t.Errorf("waves were not announced, or not soaked between:\n%s", stderr)
	}

	for _, args := range [][]string{{"reboot", "--wave", "purple"}, {"reboot", "-g", "purple"}} {
		if got := run(args); got != 1 {
			t.Errorf("%v: exit %d, want 1", args, got)
		}
	}
}

// TestRolloutPause: a wave marked pause stops the roll-out after it, and says
// how to carry on.
func TestRolloutPause(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes+`rollout:
  waves:
    - {groups: [blue], pause: true}
    - green
`)

	var got int

	stderr := captureStderr(t, func() { got = run([]string{"reboot", "-g", "worker"}) })
	if got != 0 {
		t.Fatalf("exit %d\n%s", got, stderr)
	}

	if order := rebooted(t, log); order != "10.0.0.2" {
		t.Errorf("rebooted %q before pausing, want only blue's 10.0.0.2", order)
	}

	if !strings.Contains(stderr, "continue with: talman reboot --from green --group=worker") {
		t.Errorf("the pause did not say how to carry on:\n%s", stderr)
	}
}

// Without rollout in the config, the wave flags have nothing to select from.
func TestWaveFlagsNeedARollout(t *testing.T) {
	exitFixture(t)

	if got := run([]string{"reboot", "--until", "blue"}); got != 1 {
		t.Errorf("exit %d, want 1", got)
	}
}

// TestTalosctlGroup: -g on ctl names the nodes talosctl reaches. Without it
// honoured, talosctl falls back to the talosconfig's default -- every node.
func TestTalosctlGroup(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes)

	if got := run([]string{"ctl", "-g", "blue", "get", "members"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "--nodes 10.0.0.2 get members") {
		t.Errorf("ctl -g blue did not reach blue's node alone:\n%s", calls)
	}

	if got := run([]string{"ctl", "-g", "purple", "get", "members"}); got != 1 {
		t.Errorf("an unknown group: exit %d, want 1", got)
	}
}

// runHint runs args and returns the "continue with" command it printed.
func runHint(t *testing.T, args ...string) (int, string) {
	t.Helper()

	var got int

	stderr := captureStderr(t, func() { got = run(args) })

	_, hint, ok := strings.Cut(stderr, "continue with: talman ")
	if !ok {
		return got, ""
	}

	hint, _, _ = strings.Cut(hint, "\n")

	return got, hint
}

// TestRolloutHintsCarryOn: the command a pause or --until prints has to run,
// and has to keep the selection the operator made.
func TestRolloutHintsCarryOn(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes+`rollout:
  waves:
    - {groups: [blue], pause: true}
    - [green, pink]
`)

	// The printed command runs, for a wave of several groups.
	_, hint := runHint(t, "reboot", "-g", "worker", "--until", "blue")
	if hint == "" {
		t.Fatal("no continue hint after --until")
	}

	_ = os.WriteFile(log, nil, 0o644)

	if got := run(strings.Fields(hint)); got != 0 {
		t.Errorf("the hint %q does not run: exit %d", hint, got)
	}

	if order := rebooted(t, log); order != "10.0.0.3 10.0.0.4 10.0.0.5" {
		t.Errorf("the hint rebooted %q, want the waves after blue", order)
	}

	// --until survives a pause before it.
	_, hint = runHint(t, "reboot", "-g", "worker", "--until", "green")
	if !strings.Contains(hint, "--until=green") {
		t.Errorf("the pause hint %q dropped --until", hint)
	}
}

// TestRolloutPauseOnlyAfterChange: a canary wave that is already done does
// not stop every later run at itself.
func TestRolloutPauseOnlyAfterChange(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes+`rollout:
  waves:
    - {groups: [blue], pause: true}
`)

	// Every node already runs the configured version: nothing to do in
	// blue, so no reason to stop there.
	if got := run([]string{"upgrade", "--detailed-exit-code"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	t.Setenv("STUB_STALE", "10.0.0.5")

	_ = os.WriteFile(log, nil, 0o644)

	if got := run([]string{"upgrade", "--detailed-exit-code"}); got != 2 {
		calls, _ := os.ReadFile(log)
		t.Errorf("exit %d, want 2: the stale node after the done canary was not reached\n%s", got, calls)
	}
}

// TestWaveSelectionThatSelectsNothing is an error, not a quiet success.
func TestWaveSelectionThatSelectsNothing(t *testing.T) {
	exitFixtureWith(t, waveNodes+`rollout:
  waves: [controlplane, blue, green]
`)

	for _, args := range [][]string{
		{"upgrade", "--from", "green", "--until", "blue"},
		{"upgrade", "-g", "pink", "--wave", "blue"},
	} {
		if got := run(args); got != 1 {
			t.Errorf("%v: exit %d, want 1", args, got)
		}
	}
}

// TestApplyNoChangeSkipsSoakAndGate: nodes that answer "no changes" leave a
// wave with nothing to watch, so apply neither soaks nor gates after it.
func TestApplyNoChangeSkipsSoakAndGate(t *testing.T) {
	dir, log := exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
rollout:
  soak: 1h
  waves: [controlplane]
`)

	if err := os.WriteFile(filepath.Join(dir, "clusterconfig", "w1.yaml"), []byte("version: v1alpha1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan int, 1)

	go func() {
		done <- run([]string{"apply", "--no-render", "--redact-secrets=false", "--wait=false", "--health"})
	}()

	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("exit %d", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("apply soaked after a wave that changed nothing")
	}

	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), " health ") {
		t.Errorf("the health gate ran after a wave that changed nothing:\n%s", calls)
	}
}

// A mistyped wave is refused before apply renders anything: rendering
// decrypts the bundle, which may be a KMS call or a hardware-key touch.
func TestApplyChecksWavesFirst(t *testing.T) {
	dir, log := exitFixtureWith(t, waveNodes+`rollout:
  waves: [blue]
`)

	// A bundle, so that rendering would get as far as talosctl.
	if err := os.WriteFile(filepath.Join(dir, "secrets.sops.yaml"), []byte("cluster: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := run([]string{"apply", "--from", "gren"}); got != 1 {
		t.Errorf("exit %d, want 1", got)
	}

	if calls, _ := os.ReadFile(log); strings.Contains(string(calls), "gen ") {
		t.Errorf("apply rendered before rejecting --from:\n%s", calls)
	}
}

// A control plane named with -n is reached the way it answers, as one talman
// found for itself is: here only through the talosconfig's endpoints.
func TestSnapshotOfANamedNodeBehindEndpoints(t *testing.T) {
	_, log := exitFixture(t)

	t.Setenv("STUB_DEAD_DIRECT", "1")

	if got := run([]string{"etcd", "snapshot", "-n", "c1"}); got != 0 {
		calls, _ := os.ReadFile(log)
		t.Errorf("exit %d\n%s", got, calls)
	}
}

// A -g that matches no node -- a role nobody has -- selects nothing, and must
// say so: passed through to ctl as "no --nodes" it would reach every node.
func TestGroupSelectingNothingIsAnError(t *testing.T) {
	_, log := exitFixture(t) // a control plane, and no workers

	for _, args := range [][]string{
		{"ctl", "-g", "worker", "reboot"},
		{"reboot", "-g", "worker"},
		{"status", "--offline", "-g", "worker"},
	} {
		if got := run(args); got != 1 {
			t.Errorf("%v: exit %d, want 1", args, got)
		}
	}

	calls, _ := os.ReadFile(log)
	if strings.Contains(string(calls), "reboot") {
		t.Errorf("a reboot reached talosctl:\n%s", calls)
	}
}

// A snapshot landing on the same name while this one streams is not
// replaced. (The last gap, between a check and a rename, is closed by placing
// the file with a link, which no stub can race; this holds the behaviour.)
func TestSnapshotNeverReplacesOneThatAppeared(t *testing.T) {
	dir, _ := exitFixture(t)

	path := filepath.Join(dir, "snap.db")
	t.Setenv("STUB_RACE", path)

	if got := run([]string{"etcd", "snapshot", path}); got != 1 {
		t.Errorf("exit %d, want 1", got)
	}

	if b, _ := os.ReadFile(path); string(b) != "someone else's\n" {
		t.Errorf("the snapshot that appeared was replaced with %q", b)
	}
}

// An empty -n or -g -- `-n "$NODE"` with NODE unset -- is a mistake, not a
// request for every node: reset -n "" --yes wiped a whole cluster.
func TestEmptySelectorIsRefused(t *testing.T) {
	_, log := exitFixtureWith(t, waveNodes)

	for _, args := range [][]string{
		{"reset", "-n", "", "--yes"},
		{"reset", "--node=", "--yes"},
		{"upgrade", "-g", "", "--force"},
		{"reboot", "-n", " "},
		{"bootstrap", "-n", ""},
		{"status", "--offline", "-g="},
	} {
		if got := run(args); got != 1 {
			t.Errorf("%q: exit %d, want 1", args, got)
		}
	}

	calls, _ := os.ReadFile(log)
	for _, verb := range []string{" reset ", " upgrade --nodes", " reboot --nodes", " bootstrap"} {
		if strings.Contains(string(calls), verb) {
			t.Errorf("%q reached talosctl:\n%s", verb, calls)
		}
	}
}
