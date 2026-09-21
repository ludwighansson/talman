package talosctl

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// Stream failures are reported the same way captured ones are: an apply
// carries absolute config paths, and repeating them buries the streamed
// output that already explained the failure.
func TestSubcommandSharedBetweenOutputAndStream(t *testing.T) {
	args := []string{
		"--talosconfig", "/long/path/clusterconfig/talosconfig",
		"apply-config", "--nodes", "10.0.0.11",
		"--file", "/long/path/clusterconfig/node.yaml", "--mode", "auto",
	}

	if got := subcommand(args); got != "apply-config" {
		t.Errorf("subcommand() = %q, want %q", got, "apply-config")
	}
}

func TestParseKubeletVersion(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "kubelet spec",
			out: "node: 10.0.0.11\nmetadata:\n    id: kubelet\nspec:\n" +
				"    image: ghcr.io/siderolabs/kubelet:v1.37.0\n" +
				"    args:\n        - --bootstrap-kubeconfig=/etc/kubernetes/bootstrap-kubeconfig\n",
			want: "v1.37.0",
		},
		{
			name: "unprefixed tag",
			out:  "spec:\n    image: ghcr.io/siderolabs/kubelet:1.37.0\n",
			want: "v1.37.0",
		},
		{
			name: "pre-release tag",
			out:  "spec:\n    image: ghcr.io/siderolabs/kubelet:v1.38.0-rc.1\n",
			want: "v1.38.0-rc.1",
		},
		{
			name: "digest-pinned image: the digest is not part of the version",
			out:  "spec:\n    image: ghcr.io/siderolabs/kubelet:v1.37.0@sha256:0bad1dea\n",
			want: "v1.37.0",
		},
		{
			name: "another image alongside it",
			out: "spec:\n    image: ghcr.io/siderolabs/kubelet:v1.37.0\n" +
				"    args:\n        - --pod-infra-container-image=registry.k8s.io/pause:3.10\n",
			want: "v1.37.0",
		},
		{
			name: "no kubelet image present",
			out:  "spec:\n    image: registry.k8s.io/pause:3.10\n",
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
			if got := parseKubeletVersion([]byte(tt.out)); got != tt.want {
				t.Errorf("parseKubeletVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The version floor exists so an old binary is refused before an operation
// stops halfway through with talosctl's own words about an unknown flag.
func TestOlderThan(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "the floor itself", version: "v1.14.0", want: false},
		{name: "one patch below", version: "v1.13.9", want: true},
		{name: "a whole minor below", version: "v1.9.0", want: true},
		{name: "a major below", version: "v0.14.0", want: true},
		{name: "newer patch", version: "v1.14.1", want: false},
		{name: "newer minor", version: "v1.15.0", want: false},
		{name: "unprefixed", version: "1.13.0", want: true},
		{
			// A pre-release of a newer version is newer: the question is
			// whether the flags exist, not whether the build is final.
			name:    "pre-release of a newer minor",
			version: "v1.15.0-alpha.1",
			want:    false,
		},
		{
			// Anything unparseable runs: a distribution's own version string
			// should not stop talman on a comparison it could not make.
			name:    "not a version talman understands",
			version: "talosctl-from-somewhere",
			want:    false,
		},
		{name: "empty", version: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := olderThan(tt.version, MinVersion); got != tt.want {
				t.Errorf("olderThan(%q, %q) = %v, want %v", tt.version, MinVersion, got, tt.want)
			}
		})
	}
}

// Ensure runs once however many times it is called: a pass over fifty nodes
// must not spawn fifty processes to ask the same question.
func TestEnsureChecksOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	bin := filepath.Join(dir, "talosctl")

	script := "#!/bin/sh\necho x >> " + log + "\nprintf 'Client:\\n\\tTag:\\tv1.14.1\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	r := New(bin)

	for range 3 {
		if err := r.Ensure(); err != nil {
			t.Fatalf("Ensure() = %v", err)
		}
	}

	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}

	if calls := strings.Count(string(body), "x"); calls != 1 {
		t.Errorf("Ensure asked %d times, want 1", calls)
	}
}

// An old binary is named, with what talman needs from a newer one.
func TestEnsureRefusesAnOldTalosctl(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}

	bin := filepath.Join(t.TempDir(), "talosctl")

	script := "#!/bin/sh\nprintf 'Client:\\n\\tTag:\\tv1.9.5\\n'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // a test fixture
		t.Fatal(err)
	}

	err := New(bin).Ensure()
	if err == nil {
		t.Fatal("expected an error for a talosctl older than the floor")
	}

	for _, want := range []string{"v1.9.5", MinVersion, "--wipe-labels"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
