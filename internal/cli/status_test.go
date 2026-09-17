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
				{Mode: talosctl.ModeMaintenance},
				{Mode: talosctl.ModeUnreachable},
			},
			want: "3 node(s): 1 running, 1 in maintenance mode, 1 unreachable",
		},
		{
			// Modes other than running are not measured against the config:
			// a node with no version cannot be "behind" one.
			name:    "nothing answering",
			reports: []nodeReport{{Mode: talosctl.ModeUnreachable}},
			want:    "1 node(s): 1 unreachable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarise(tt.reports); got != tt.want {
				t.Errorf("summarise() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}
