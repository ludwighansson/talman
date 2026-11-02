package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/factory"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// upToDate decides whether a node gets rebooted, so "not sure" has to mean
// "upgrade", never "skip".
func TestUpToDate(t *testing.T) {
	const (
		schematic = "079113ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae"
		other     = "aaaa13ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae"
	)

	tests := []struct {
		name          string
		current       talosctl.NodeState
		wantVersion   string
		wantSchematic string
		want          bool
	}{
		{
			name:          "same version and schematic",
			current:       talosctl.NodeState{TalosVersion: "v1.14.0", SchematicID: schematic},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          true,
		},
		{
			name:          "older version",
			current:       talosctl.NodeState{TalosVersion: "v1.13.5", SchematicID: schematic},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          false,
		},
		{
			name:          "same version, different schematic (an extension was added)",
			current:       talosctl.NodeState{TalosVersion: "v1.14.0", SchematicID: other},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          false,
		},
		{
			name:          "version unknown: must not be read as agreement",
			current:       talosctl.NodeState{SchematicID: schematic},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          false,
		},
		{
			name:          "no schematic reported, vanilla desired",
			current:       talosctl.NodeState{TalosVersion: "v1.14.0"},
			wantVersion:   "v1.14.0",
			wantSchematic: factory.VanillaID,
			want:          true,
		},
		{
			name:          "no schematic reported but a custom one desired",
			current:       talosctl.NodeState{TalosVersion: "v1.14.0"},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          false,
		},
		{
			name:          "nothing known at all",
			current:       talosctl.NodeState{},
			wantVersion:   "v1.14.0",
			wantSchematic: schematic,
			want:          false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upToDate(tt.current, tt.wantVersion, tt.wantSchematic); got != tt.want {
				t.Errorf("upToDate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Same rule as the Talos check, one version deep: a kubelet talman could not
// read must never be mistaken for one already on the target.
func TestK8sUpToDate(t *testing.T) {
	tests := []struct {
		name    string
		current string
		want    string
		up      bool
	}{
		{
			name:    "same version",
			current: "v1.37.0",
			want:    "1.37.0",
			up:      true,
		},
		{
			name:    "same version, both v-prefixed",
			current: "v1.37.0",
			want:    "v1.37.0",
			up:      true,
		},
		{
			name:    "older kubelet",
			current: "v1.36.2",
			want:    "1.37.0",
			up:      false,
		},
		{
			name:    "newer kubelet than the config asks for",
			current: "v1.38.0",
			want:    "1.37.0",
			up:      false,
		},
		{
			name:    "patch release differs",
			current: "v1.37.1",
			want:    "1.37.0",
			up:      false,
		},
		{
			name:    "version unknown: must not be read as agreement",
			current: "",
			want:    "1.37.0",
			up:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := k8sUpToDate(tt.current, tt.want); got != tt.up {
				t.Errorf("k8sUpToDate(%q, %q) = %v, want %v", tt.current, tt.want, got, tt.up)
			}
		})
	}
}

// upgrade-k8s --to a version the config does not name leaves a cluster the
// next apply would take back down, and has to say so; one that agrees with
// the config, or a dry run, has nothing to warn about.
func TestUpgradeK8sToAnotherVersionWarns(t *testing.T) {
	exitFixture(t) // kubernetesVersion: v1.37.0

	for _, tt := range []struct {
		args []string
		warn bool
	}{
		{[]string{"upgrade-k8s", "--to", "v1.38.0"}, true},
		{[]string{"upgrade-k8s", "--to", "1.37.0"}, false},
		{[]string{"upgrade-k8s"}, false},
		{[]string{"upgrade-k8s", "--to", "v1.38.0", "--dry-run"}, false},
	} {
		var got int

		stderr := captureStderr(t, func() { got = run(tt.args) })
		if got != 0 {
			t.Errorf("%v: exit %d\n%s", tt.args, got, stderr)
		}

		if warned := strings.Contains(stderr, "still says kubernetesVersion: v1.37.0"); warned != tt.warn {
			t.Errorf("%v: warned %t, want %t:\n%s", tt.args, warned, tt.warn, stderr)
		}
	}
}

// A dry run that cannot read a node says so, and offers nothing it would
// refuse: --force cannot be combined with --dry-run.
func TestUpgradeDryRunOfAnUnreadableNode(t *testing.T) {
	exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
`)

	t.Setenv("STUB_FAIL", "10.0.0.2")

	var got int

	stderr := captureStderr(t, func() { got = run([]string{"upgrade", "--dry-run"}) })
	if got != 1 {
		t.Fatalf("exit %d, want 1\n%s", got, stderr)
	}

	if strings.Contains(stderr, "--force") || strings.Contains(stderr, "not upgraded") {
		t.Errorf("a dry run offered --force, or spoke of nodes not upgraded:\n%s", stderr)
	}

	if !strings.Contains(stderr, "1 node(s) were not checked: w1") {
		t.Errorf("want the node the dry run did not check named:\n%s", stderr)
	}
}

// newer decides what is refused as a downgrade, so a version it cannot read
// has to mean "go ahead", never "refuse".
func TestNewer(t *testing.T) {
	for _, tt := range []struct {
		running, want string
		newer         bool
	}{
		{"v1.14.1", "v1.13.0", true},
		{"v1.14.1", "v1.14.0", true},
		{"v1.14.1", "v1.14.1", false},
		{"v1.13.5", "v1.14.0", false},
		// A release is past its own prereleases.
		{"v1.14.0", "v1.14.0-beta.1", true},
		{"v1.14.0-beta.1", "v1.14.0", false},
		// The kubelet's tag and the config's version are written either way.
		{"1.37.1", "v1.37.0", true},
		{"v1.37.0", "1.37.0", false},
		// Not read, or not a version: no answer.
		{"", "v1.14.0", false},
		{"v1.14.1", "latest", false},
	} {
		if got := newer(tt.running, tt.want); got != tt.newer {
			t.Errorf("newer(%q, %q) = %t, want %t", tt.running, tt.want, got, tt.newer)
		}
	}
}

// TestUpgradeRefusesDowngrades: a node past the Talos it is configured for is
// refused before any node is upgraded -- --force and --dry-run included --
// and named with both versions. --allow-downgrades goes ahead.
func TestUpgradeRefusesDowngrades(t *testing.T) {
	_, log := exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
`)

	// c1 behind its version, w1 past it: refusing at w1 would come after c1
	// had been upgraded and rebooted.
	t.Setenv("STUB_STALE", "10.0.0.1")
	t.Setenv("STUB_NEWER", "10.0.0.2")

	for _, args := range [][]string{
		{"upgrade"},
		{"upgrade", "--force"},
		{"upgrade", "--dry-run"},
	} {
		_ = os.WriteFile(log, nil, 0o644)

		var got int

		stderr := captureStderr(t, func() { got = run(args) })
		if got != 1 {
			t.Errorf("%v: exit %d, want 1\n%s", args, got, stderr)
		}

		if !strings.Contains(stderr, "upgrading would downgrade Talos on 1 node(s): w1 v1.15.0 -> v1.14.1\n"+
			"  raise talosVersion to what they run, or pass --allow-downgrades") {
			t.Errorf("%v: want the downgrade named, and the way past it:\n%s", args, stderr)
		}

		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), " upgrade --nodes") {
			t.Errorf("%v: upgraded a node before refusing:\n%s", args, calls)
		}
	}

	_ = os.WriteFile(log, nil, 0o644)

	if got := run([]string{"upgrade", "--allow-downgrades"}); got != 0 {
		t.Fatalf("--allow-downgrades: exit %d", got)
	}

	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), " upgrade --nodes 10.0.0.2") {
		t.Errorf("--allow-downgrades did not downgrade w1:\n%s", calls)
	}
}

// A node's own talosVersion is its target, and the one a downgrade is
// measured against.
func TestUpgradeDowngradeOfAPinnedNode(t *testing.T) {
	exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
    talosVersion: v1.13.0
`)

	var got int

	stderr := captureStderr(t, func() { got = run([]string{"upgrade"}) })
	if got != 1 || !strings.Contains(stderr, "on 1 node(s): w1 v1.14.1 -> v1.13.0") {
		t.Errorf("exit %d, want w1 refused against its own version:\n%s", got, stderr)
	}
}

// TestUpgradeK8sRefusesDowngrades: a kubelet newer than the target is
// refused, whether the target is the config's or --to's and whatever --force
// says. talosctl refuses only a downgrade across minor versions.
func TestUpgradeK8sRefusesDowngrades(t *testing.T) {
	_, log := exitFixture(t) // kubernetesVersion: v1.37.0

	for _, tt := range []struct {
		kubelet string
		args    []string
		refused string
	}{
		{"v1.37.1", []string{"upgrade-k8s"}, "c1 v1.37.1 -> v1.37.0\n  raise kubernetesVersion to what they run"},
		{"v1.37.1", []string{"upgrade-k8s", "--force"}, "c1 v1.37.1 -> v1.37.0"},
		{"v1.37.1", []string{"upgrade-k8s", "--dry-run"}, "c1 v1.37.1 -> v1.37.0"},
		{"v1.37.0", []string{"upgrade-k8s", "--to", "1.36.2"}, "c1 v1.37.0 -> v1.36.2\n  use a --to no older"},
		{"v1.37.0", []string{"upgrade-k8s", "--to", "v1.38.0"}, ""},
		{"v1.37.1", []string{"upgrade-k8s", "--allow-downgrades"}, ""},
	} {
		t.Setenv("STUB_KUBELET", tt.kubelet)

		_ = os.WriteFile(log, nil, 0o644)

		var got int

		stderr := captureStderr(t, func() { got = run(tt.args) })
		calls, _ := os.ReadFile(log)
		ran := strings.Contains(string(calls), " upgrade-k8s ")

		switch {
		case tt.refused == "" && (got != 0 || !ran):
			t.Errorf("%v on %s: exit %d, want the upgrade run\n%s", tt.args, tt.kubelet, got, stderr)
		case tt.refused != "" && (got != 1 || ran):
			t.Errorf("%v on %s: exit %d, want it refused before talosctl ran\n%s", tt.args, tt.kubelet, got, stderr)
		case tt.refused != "" && !strings.Contains(stderr, "upgrading would downgrade Kubernetes on 1 node(s): "+tt.refused):
			t.Errorf("%v on %s: want %q:\n%s", tt.args, tt.kubelet, tt.refused, stderr)
		}
	}
}

// TestApplyRefusesKubernetesDowngrade: every config carries
// kubernetesVersion, and Talos moves a node to it older or not, so a cluster
// past it gets nothing sent -- not even a dry run's.
func TestApplyRefusesKubernetesDowngrade(t *testing.T) {
	_, log := exitFixture(t) // kubernetesVersion: v1.37.0

	t.Setenv("STUB_KUBELET", "v1.37.1")

	for _, args := range [][]string{
		{"apply", "--no-render", "--redact-secrets=false", "--wait=false"},
		{"apply", "--no-render", "--redact-secrets=false", "--dry-run"},
		{"apply", "--no-render", "--redact-secrets=false", "--mode=staged"},
	} {
		_ = os.WriteFile(log, nil, 0o644)

		var got int

		stderr := captureStderr(t, func() { got = run(args) })
		if got != 1 || !strings.Contains(stderr, "applying would downgrade Kubernetes on 1 node(s): c1 v1.37.1 -> v1.37.0") {
			t.Errorf("%v: exit %d, want the downgrade refused:\n%s", args, got, stderr)
		}

		if calls, _ := os.ReadFile(log); strings.Contains(string(calls), " apply-config ") {
			t.Errorf("%v: sent a config before refusing:\n%s", args, calls)
		}
	}

	_ = os.WriteFile(log, nil, 0o644)

	if got := run([]string{"apply", "--no-render", "--redact-secrets=false", "--wait=false",
		"--allow-downgrades"}); got != 0 {
		t.Fatalf("--allow-downgrades: exit %d", got)
	}

	if calls, _ := os.ReadFile(log); !strings.Contains(string(calls), " apply-config ") {
		t.Errorf("--allow-downgrades sent nothing:\n%s", calls)
	}

	// Up to date, or behind: nothing to refuse.
	for _, kubelet := range []string{"v1.37.0", "v1.36.4"} {
		t.Setenv("STUB_KUBELET", kubelet)

		if got := run([]string{"apply", "--no-render", "--redact-secrets=false", "--wait=false"}); got != 0 {
			t.Errorf("kubelet %s: exit %d, want 0", kubelet, got)
		}
	}
}
