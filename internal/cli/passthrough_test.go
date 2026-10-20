package cli

import (
	"strings"
	"testing"
)

// talman's own flags come off the front of a ctl command line in every
// spelling pflag accepts, and the rest goes to talosctl untouched.
func TestPassthroughArgs(t *testing.T) {
	for _, tt := range []struct {
		args        []string
		nodes, rest string
	}{
		{[]string{"-n", "w1", "dmesg"}, "w1", "dmesg"},
		{[]string{"-nw1", "dmesg"}, "w1", "dmesg"},
		{[]string{"-n=w1", "dmesg"}, "w1", "dmesg"},
		{[]string{"--node", "w1,w2", "dmesg"}, "w1 w2", "dmesg"},
		{[]string{"--node=w1", "--", "-e", "x", "version"}, "w1", "-e x version"},
		{[]string{"--endpoints", "1.2.3.4", "version"}, "", "--endpoints 1.2.3.4 version"},
	} {
		saved := opts
		nodes, rest, _, err := passthroughArgs(tt.args)
		opts = saved

		if err != nil {
			t.Errorf("%v: %v", tt.args, err)

			continue
		}

		if got := strings.Join(nodes, " "); got != tt.nodes {
			t.Errorf("%v: nodes %q, want %q", tt.args, got, tt.nodes)
		}

		if got := strings.Join(rest, " "); got != tt.rest {
			t.Errorf("%v: rest %q, want %q", tt.args, got, tt.rest)
		}
	}
}

// ctl's -n and -g complete as every other command's do, though cobra parses
// none of its flags; past them, a -n is talosctl's own --nodes and is left to
// the shell.
func TestPassthroughCompletesItsOwnFlags(t *testing.T) {
	exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
    groups: [blue]
`)

	complete := func(args ...string) string {
		return captureStdout(t, func() { run(append([]string{"__complete"}, args...)) })
	}

	for _, args := range [][]string{{"ctl", "-n", ""}, {"talosctl", "-v", "--node", ""}} {
		if out := complete(args...); !strings.Contains(out, "c1\tcontrolplane") || !strings.Contains(out, "w1\tworker") {
			t.Errorf("%v completed\n%s\nwant the hostnames", args, out)
		}
	}

	if out := complete("ctl", "-g", ""); !strings.Contains(out, "blue\tgroup") {
		t.Errorf("ctl -g completed\n%s\nwant the groups", out)
	}

	if out := complete("ctl", "logs", "-n", ""); strings.Contains(out, "c1") || !strings.Contains(out, ":0\n") {
		t.Errorf("talosctl's own -n completed\n%s\nwant it left to the shell", out)
	}
}
