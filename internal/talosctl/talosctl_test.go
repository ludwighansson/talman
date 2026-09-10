package talosctl

import (
	"errors"
	"strings"
	"testing"
)

// ExitError has to name the command that failed. ConfigEndpoint and
// ConfigNode both build argv starting with --talosconfig, which used to make
// the message read "talosctl (no subcommand)".
func TestExitErrorNamesTheSubcommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "plain subcommand",
			args: []string{"gen", "config", "c", "https://x", "--with-secrets", "/tmp/s"},
			want: "talosctl gen config:",
		},
		{
			name: "leading global flag with a separate value",
			args: []string{"--talosconfig", "/tmp/tc", "config", "endpoint", "10.0.0.1"},
			want: "talosctl config endpoint:",
		},
		{
			name: "leading global flag in --flag=value form",
			args: []string{"--talosconfig=/tmp/tc", "bootstrap", "--nodes", "10.0.0.1"},
			want: "talosctl bootstrap:",
		},
		{
			name: "single-word subcommand",
			args: []string{"validate", "--config", "x.yaml", "--mode", "metal"},
			want: "talosctl validate:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &ExitError{Args: tt.args, Stderr: "boom", Err: errors.New("exit status 1")}

			got := err.Error()
			if !strings.HasPrefix(got, tt.want) {
				t.Errorf("Error() = %q, want it to start with %q", got, tt.want)
			}

			if !strings.Contains(got, "boom") {
				t.Errorf("Error() dropped stderr: %q", got)
			}
		})
	}
}

// Without stderr there is still an error to report.
func TestExitErrorFallsBackToTheProcessError(t *testing.T) {
	err := &ExitError{Args: []string{"health"}, Err: errors.New("exit status 2")}

	if got := err.Error(); !strings.Contains(got, "exit status 2") {
		t.Errorf("Error() = %q, want the process error", got)
	}

	if !errors.Is(err, err.Err) {
		t.Error("ExitError does not unwrap to its cause")
	}
}

// TestParseServerTag: `talosctl version` prints a Client block before the
// Server block, each with a Tag. Taking the first would report the local
// talosctl version as the node's and make every upgrade look unnecessary.
func TestParseServerTag(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "client block precedes server block",
			out: "Client:\n\tTag:         v1.14.0\n\tSHA:         undefined\n" +
				"Server:\n\tNODE:        10.0.0.11\n\tTag:         v1.13.5\n\tSHA:         abcdef\n",
			want: "v1.13.5",
		},
		{
			name: "server tag lacking the v prefix is normalised",
			out:  "Client:\n\tTag:         v1.14.0\nServer:\n\tTag:         1.13.5\n",
			want: "v1.13.5",
		},
		{
			name: "client only, node unreachable",
			out:  "Client:\n\tTag:         v1.14.0\n",
			want: "",
		},
		{
			name: "empty",
			out:  "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseServerTag(tt.out); got != tt.want {
				t.Errorf("parseServerTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseSchematicID(t *testing.T) {
	const id = "079113ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae"

	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "schematic among other extensions",
			out: "node: 10.0.0.11\nmetadata:\n    id: \"0\"\nspec:\n    image: drbd.sqsh\n" +
				"    metadata:\n        name: drbd\n        version: v9.2.0\n" +
				"---\nnode: 10.0.0.11\nmetadata:\n    id: \"1\"\nspec:\n" +
				"    metadata:\n        name: schematic\n        version: " + id + "\n",
			want: id,
		},
		{
			name: "no schematic extension present",
			out:  "spec:\n    metadata:\n        name: drbd\n        version: v9.2.0\n",
			want: "",
		},
		{
			name: "empty output",
			out:  "",
			want: "",
		},
		{
			name: "unexpected shape degrades to unknown rather than guessing",
			out:  "just: a scalar\n",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseSchematicID([]byte(tt.out)); got != tt.want {
				t.Errorf("parseSchematicID() = %q, want %q", got, tt.want)
			}
		})
	}
}
