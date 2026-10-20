package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/interrupt"
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

// Ctrl-C is how `talman ctl logs -f` is stopped, and talosctl takes it as
// that: it exits 0. talman reports the interrupt as any run does, and not as
// a failure of talosctl's. Only a real signal does that, so the test runs
// itself again to send one.
func TestPassthroughStoppedByCtrlC(t *testing.T) {
	if os.Getenv("TALMAN_CTL_INTERRUPT_CHILD") == "1" {
		stop := interrupt.Watch()
		code := run([]string{"ctl", "logs", "kubelet", "-f"})

		stop()
		fmt.Fprintf(os.Stderr, "exit=%d\n", code)
		os.Exit(0)
	}

	dir, _ := exitFixture(t)

	// A talosctl following logs: the terminal's Ctrl-C reaches talman, which
	// passes it on, and talosctl stops cleanly.
	stub := `#!/bin/sh
case " $* " in
*" version --client "*) printf 'Client:\n\tTag:         v1.14.1\n'; exit 0 ;;
esac
trap 'exit 0' TERM INT
kill -INT $PPID
while :; do sleep 0.05; done
`
	if err := os.WriteFile(filepath.Join(dir, "talosctl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestPassthroughStoppedByCtrlC$")
	cmd.Env = append(os.Environ(), "TALMAN_CTL_INTERRUPT_CHILD=1")

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}

	if got := stderr.String(); strings.Contains(got, "error:") || !strings.Contains(got, "exit=130") {
		t.Errorf("want a run stopped by Ctrl-C, exit 130, and no error:\n%s", got)
	}
}
