package cli

import (
	"strings"
	"testing"
)

// A mistyped subcommand fails the way a mistyped command does: a CI job
// that runs `talman secrets generat` must not read help text as success.
func TestGroupsRefuseUnknownSubcommands(t *testing.T) {
	for _, tt := range []struct {
		args    []string
		suggest string
	}{
		{[]string{"secrets", "generat"}, "generate"},
		{[]string{"etcd", "snapshots"}, "snapshot"},
		{[]string{"schematic", "ids"}, "id"},
		{[]string{"image", "urls"}, "url"},
		{[]string{"completion", "zhs"}, "zsh"},
	} {
		var code int

		stderr := captureStderr(t, func() { code = run(tt.args) })

		if code != 1 {
			t.Errorf("talman %s exited %d, want 1", strings.Join(tt.args, " "), code)
		}

		if !strings.Contains(stderr, "unknown command") || !strings.Contains(stderr, "\t"+tt.suggest) {
			t.Errorf("talman %s said\n%s\nwant an unknown command suggesting %s",
				strings.Join(tt.args, " "), stderr, tt.suggest)
		}
	}

	// A group on its own is a request for its help.
	for _, group := range []string{"secrets", "etcd", "schematic", "image", "completion"} {
		var code int

		out := captureStdout(t, func() { code = run([]string{group}) })

		if code != 0 || !strings.Contains(out, "Available Commands:") {
			t.Errorf("talman %s exited %d with\n%s\nwant its help and 0", group, code, out)
		}
	}
}
