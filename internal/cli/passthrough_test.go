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
