package cli

import (
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
