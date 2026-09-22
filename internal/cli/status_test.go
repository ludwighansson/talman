package cli

import (
	"testing"

	"github.com/ludwighansson/talman/internal/talosctl"
)

const (
	schematicA = "079113ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae"
	schematicB = "aaaa13ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae"
)

// What talman could not read must show as unread. Printing the configured
// value for a node that never answered would make the table agree with itself.
func TestStatusColumns(t *testing.T) {
	tests := []struct {
		name           string
		report         nodeReport
		talos          string
		kubernetes     string
		schematicField string
	}{
		{
			name: "a node on everything the config asks for",
			report: nodeReport{
				asked:          true,
				State:          talosctl.NodeState{TalosVersion: "v1.14.0", SchematicID: schematicA},
				K8s:            "v1.37.0",
				wantTalos:      "v1.14.0",
				wantSchematic:  schematicA,
				wantKubernetes: "v1.37.0",
			},
			talos:          "v1.14.0",
			kubernetes:     "v1.37.0",
			schematicField: "079113ce0508",
		},
		{
			name: "drift in all three",
			report: nodeReport{
				asked:          true,
				State:          talosctl.NodeState{TalosVersion: "v1.13.5", SchematicID: schematicB},
				K8s:            "v1.36.2",
				wantTalos:      "v1.14.0",
				wantSchematic:  schematicA,
				wantKubernetes: "v1.37.0",
			},
			talos:          "v1.13.5 → v1.14.0",
			kubernetes:     "v1.36.2 → v1.37.0",
			schematicField: "aaaa13ce0508 → 079113ce0508",
		},
		{
			name: "a node that answered nothing",
			report: nodeReport{
				asked:          true,
				wantTalos:      "v1.14.0",
				wantSchematic:  schematicA,
				wantKubernetes: "v1.37.0",
			},
			talos:          "-",
			kubernetes:     "-",
			schematicField: "-",
		},
		{
			// kubernetesVersion may be written either way in the config, and
			// the kubelet image tag is v-prefixed. That is not drift.
			name: "an unprefixed kubernetesVersion is the same version",
			report: nodeReport{
				asked:          true,
				K8s:            "v1.37.0",
				wantKubernetes: "1.37.0",
			},
			talos:          "-",
			kubernetes:     "v1.37.0",
			schematicField: "-",
		},
		{
			// A node not installed from a factory image reports no schematic:
			// unknown, not drift against every ID there is.
			name: "no schematic reported",
			report: nodeReport{
				asked:         true,
				State:         talosctl.NodeState{TalosVersion: "v1.14.0"},
				wantTalos:     "v1.14.0",
				wantSchematic: schematicA,
			},
			talos:          "v1.14.0",
			kubernetes:     "-",
			schematicField: "-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.report.talos(); got != tt.talos {
				t.Errorf("talos() = %q, want %q", got, tt.talos)
			}

			if got := tt.report.kubernetes(); got != tt.kubernetes {
				t.Errorf("kubernetes() = %q, want %q", got, tt.kubernetes)
			}

			if got := tt.report.schematic(); got != tt.schematicField {
				t.Errorf("schematic() = %q, want %q", got, tt.schematicField)
			}
		})
	}
}

func TestSummarise(t *testing.T) {
	current := nodeReport{
		asked:          true,
		Mode:           talosctl.ModeRunning,
		State:          talosctl.NodeState{TalosVersion: "v1.14.0", SchematicID: schematicA},
		K8s:            "v1.37.0",
		wantTalos:      "v1.14.0",
		wantSchematic:  schematicA,
		wantKubernetes: "v1.37.0",
	}

	stale := current
	stale.State.TalosVersion = "v1.13.5"

	behindOnK8s := current
	behindOnK8s.K8s = "v1.36.2"

	tests := []struct {
		name    string
		reports []nodeReport
		want    string
	}{
		{
			name:    "a cluster with nothing to do",
			reports: []nodeReport{current, current},
			want:    "2 node(s): 2 running",
		},
		{
			name:    "one node behind on Talos",
			reports: []nodeReport{current, stale},
			want:    "2 node(s): 2 running; 1 not on the configured version",
		},
		{
			// Kubernetes drift counts too: upgrade-k8s is as much a pending
			// change as upgrade is.
			name:    "one node behind on Kubernetes alone",
			reports: []nodeReport{current, behindOnK8s},
			want:    "2 node(s): 2 running; 1 not on the configured version",
		},
		{
			name: "a cluster mid-adoption",
			reports: []nodeReport{
				current,
				{asked: true, Mode: talosctl.ModeMaintenance},
				{asked: true, Mode: talosctl.ModeUnreachable},
			},
			want: "3 node(s): 1 running, 1 in maintenance mode, 1 unreachable",
		},
		{
			// Modes other than running are not measured against the config:
			// a node with no version cannot be "behind" one.
			name:    "nothing answering",
			reports: []nodeReport{{asked: true, Mode: talosctl.ModeUnreachable}},
			want:    "1 node(s): 1 unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarise(tt.reports, false); got != tt.want {
				t.Errorf("summarise() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// The table keeps its shape whether or not the cluster was asked: the columns
// do not move, only what can honestly be put in them. A dash is what talman
// could not establish; a node that was never contacted is not a node that
// answered nothing.
func TestOfflineKeepsTheShape(t *testing.T) {
	asked := nodeReport{
		asked:          true,
		Mode:           talosctl.ModeRunning,
		State:          talosctl.NodeState{TalosVersion: "v1.13.5", SchematicID: schematicB},
		K8s:            "v1.36.2",
		wantTalos:      "v1.14.1",
		wantSchematic:  schematicA,
		wantKubernetes: "v1.37.0",
	}

	notAsked := asked
	notAsked.asked = false
	notAsked.Mode = talosctl.ModeUnreachable // whatever is in the field is not an answer
	notAsked.State = talosctl.NodeState{}
	notAsked.K8s = ""

	t.Run("asked: what is running, against what is wanted", func(t *testing.T) {
		if got := asked.status(); got != "running" {
			t.Errorf("status() = %q, want running", got)
		}

		if got := asked.talos(); got != "v1.13.5 → v1.14.1" {
			t.Errorf("talos() = %q, want the drift", got)
		}

		if got := asked.kubernetes(); got != "v1.36.2 → v1.37.0" {
			t.Errorf("kubernetes() = %q, want the drift", got)
		}
	})

	t.Run("not asked: a dash where only a node could have answered", func(t *testing.T) {
		if got := notAsked.status(); got != "-" {
			t.Errorf("status() = %q, want a dash: nothing was asked", got)
		}

		// The config's own answers stay, because they are knowable without a
		// cluster and dropping them would make --offline useless for reading
		// a config.
		if got := notAsked.talos(); got != "v1.14.1" {
			t.Errorf("talos() = %q, want what the config asks for", got)
		}

		if got := notAsked.kubernetes(); got != "v1.37.0" {
			t.Errorf("kubernetes() = %q, want what the config asks for", got)
		}

		if got := notAsked.schematic(); got != short(schematicA) {
			t.Errorf("schematic() = %q, want the configured one", got)
		}
	})

	t.Run("the offline summary counts, and claims nothing else", func(t *testing.T) {
		got := summarise([]nodeReport{notAsked, notAsked}, true)

		if got != "2 node(s)" {
			t.Errorf("summarise(offline) = %q, want a bare count", got)
		}
	})
}
