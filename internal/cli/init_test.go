package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
)

func TestInit(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	dir := filepath.Join(t.TempDir(), "prod")
	args := []string{"init", dir, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0",
		"--controlplane", "cp-01=10.0.0.11", "--worker", "w-01=10.0.0.21", "--age", "age1example"}

	if got := run(args); got != 0 {
		t.Fatalf("exit %d", got)
	}

	cfg, err := config.Load(filepath.Join(dir, config.DefaultFileName))
	if err != nil {
		t.Fatalf("init wrote a config that does not load: %v", err)
	}

	switch {
	case cfg.ClusterName != "prod":
		t.Errorf("clusterName = %q, want the directory's name", cfg.ClusterName)
	case cfg.Endpoint != "https://10.0.0.11:6443":
		t.Errorf("endpoint = %q, want the first control plane's", cfg.Endpoint)
	case cfg.KubernetesVersion != "v1.37.0":
		t.Errorf("kubernetesVersion = %q", cfg.KubernetesVersion)
	case len(cfg.Nodes) != 2 || cfg.Nodes[1].Role != config.RoleWorker:
		t.Errorf("nodes = %+v", cfg.Nodes)
	}

	sops, err := os.ReadFile(filepath.Join(dir, ".sops.yaml"))
	if err != nil || !strings.Contains(string(sops), "age: age1example") {
		t.Errorf(".sops.yaml = %q, %v", sops, err)
	}

	if got := run(args); got != 1 {
		t.Errorf("a second init over the same directory gave exit %d, want 1", got)
	}

	bad := filepath.Join(t.TempDir(), "bad")
	if got := run([]string{"init", bad, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0",
		"--controlplane", "CP=10.0.0.1"}); got != 1 {
		t.Errorf("an invalid hostname gave exit %d, want 1", got)
	}

	if exists(bad) {
		t.Error("a failed init left its directory behind")
	}

	if got := run([]string{"init", bad, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0"}); got != 1 {
		t.Errorf("init with no control plane gave exit %d, want 1", got)
	}
}

func TestInitIPv6(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	dir := filepath.Join(t.TempDir(), "v6")

	if got := run([]string{"init", dir, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0",
		"--controlplane", "cp=fd00::11"}); got != 0 {
		t.Fatalf("exit %d", got)
	}

	cfg, err := config.Load(filepath.Join(dir, config.DefaultFileName))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Endpoint != "https://[fd00::11]:6443" {
		t.Errorf("endpoint = %q, want the address bracketed", cfg.Endpoint)
	}
}

// Values init writes land in the file as they were given: a '#' is not the
// start of a comment, and a ': ' does not break the file.
func TestInitQuotes(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	for _, name := range []string{"prod #2", "eu: prod"} {
		dir := filepath.Join(t.TempDir(), "c")

		// Refused by clusterName validation, as it should be -- but refused
		// by name, not truncated to "prod" or rejected as broken YAML.
		stderr := captureStderr(t, func() {
			if got := run([]string{"init", dir, "--cluster-name", name, "--talos-version", "v1.14.1",
				"--kubernetes-version", "1.37.0", "--controlplane", "cp=10.0.0.1"}); got != 1 {
				t.Errorf("--cluster-name %q: exit %d, want 1", name, got)
			}
		})

		if !strings.Contains(stderr, "clusterName "+strconv.Quote(name)) {
			t.Errorf("--cluster-name %q was not refused by name:\n%s", name, stderr)
		}
	}

	dir := filepath.Join(t.TempDir(), "c")

	if got := run([]string{"init", dir, "--cluster-name", "prod", "--talos-version", "Client:\n\tTag: x",
		"--kubernetes-version", "1.37.0", "--controlplane", "cp=10.0.0.1"}); got != 1 {
		t.Errorf("a version that is not one: exit %d, want 1", got)
	}
}

// A refused init leaves nothing it made, and nothing it did not make is
// removed: the directory was there before, so it stays.
func TestInitIsAllOrNothing(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	dir := filepath.Join(t.TempDir(), "c")

	// Something in the way of .sops.yaml: init refuses before writing.
	if err := os.MkdirAll(filepath.Join(dir, ".sops.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}

	args := []string{"init", dir, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0",
		"--controlplane", "cp=10.0.0.1"}

	if got := run(append(args, "--age", "age1x")); got != 1 {
		t.Fatalf("exit %d, want 1", got)
	}

	if exists(filepath.Join(dir, config.DefaultFileName)) {
		t.Error("talman.yaml was left behind by a refused init")
	}

	if !exists(dir) {
		t.Error("init removed a directory it did not create")
	}

	if err := os.Remove(filepath.Join(dir, ".sops.yaml")); err != nil {
		t.Fatal(err)
	}

	if got := run(append(args, "--age", "age1x")); got != 0 {
		t.Errorf("init after the failed one: exit %d, want 0", got)
	}
}

// A failed init into a path of several new directories leaves none of them.
// The endpoint is refused only once the written file is loaded, after the
// directories exist.
func TestInitRemovesTheDirectoriesItMade(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	root := t.TempDir()

	if got := run([]string{"init", filepath.Join(root, "clusters", "prod", "eu"), "--talos-version", "v1.14.1",
		"--kubernetes-version", "1.37.0", "--controlplane", "cp-01=10.0.0.1",
		"--endpoint", "https://10.0.0.1"}); got != 1 {
		t.Fatalf("exit %d, want 1", got)
	}

	if exists(filepath.Join(root, "clusters")) {
		t.Error("a failed init left the directories it made behind")
	}
}

// A value init was handed is refused by the flag it came in on, and a name
// init took from the directory is said to be that: the operator never typed
// it.
func TestInitNamesWhatWasGiven(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	versions := []string{"--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0"}

	for want, args := range map[string][]string{
		`--worker "w-01=10.0.0.256"`:   {"--controlplane", "cp=10.0.0.1", "--worker", "w-01=10.0.0.256"},
		`--controlplane "CP=10.0.0.1"`: {"--controlplane", "CP=10.0.0.1"},
		"--cluster-name:":              {"--cluster-name", "a/b", "--controlplane", "cp=10.0.0.1"},
		"the name of the directory":    {"--controlplane", "cp=10.0.0.1"},
	} {
		dir := filepath.Join(t.TempDir(), "c")
		if want == "the name of the directory" {
			dir = filepath.Join(t.TempDir(), "My Cluster")
		}

		stderr := captureStderr(t, func() {
			if got := run(append(append([]string{"init", dir}, versions...), args...)); got != 1 {
				t.Errorf("%v: exit %d, want 1", args, got)
			}
		})

		if !strings.Contains(stderr, want) {
			t.Errorf("%v: the error does not say %s:\n%s", args, want, stderr)
		}

		if exists(dir) {
			t.Errorf("%v: a refused init left %s behind", args, dir)
		}
	}
}

// What init says comes next reaches a running cluster; a bundle with nothing
// to be encrypted to is said before it is generated, and an endpoint that is
// one control plane of several is pointed out.
func TestInitSaysWhatIsNext(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")

	root := t.TempDir()
	args := []string{"--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0",
		"--controlplane", "cp-01=10.0.0.11", "--controlplane", "cp-02=10.0.0.12"}

	var stdout string

	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if got := run(append([]string{"init", filepath.Join(root, "prod")}, args...)); got != 0 {
				t.Errorf("exit %d", got)
			}
		})
	})

	for _, want := range []string{"secrets generate", "apply --bootstrap", "creation_rules:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("init does not say %q:\n%s", want, stdout)
		}
	}

	if !strings.Contains(stderr, "VIP or a load balancer") {
		t.Errorf("an endpoint on one of two control planes was not pointed out:\n%s", stderr)
	}

	// A .sops.yaml above the directory already says how to encrypt.
	if err := os.WriteFile(filepath.Join(root, ".sops.yaml"), []byte("creation_rules: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout = captureStdout(t, func() {
		if got := run(append([]string{"init", filepath.Join(root, "dev"), "--endpoint", "https://10.0.0.10:6443"},
			args...)); got != 0 {
			t.Errorf("exit %d", got)
		}
	})

	if strings.Contains(stdout, "creation_rules:") {
		t.Errorf("init asked for a .sops.yaml with one already above it:\n%s", stdout)
	}
}

// Without talosctl, init cannot ask it for the versions to target, and says
// which flags stand in for it -- both, since either default is its answer --
// and writes the config when given them.
func TestInitWithoutTalosctl(t *testing.T) {
	t.Setenv("TALMAN_CONFIG", "")
	t.Setenv("PATH", t.TempDir())

	base := t.TempDir()

	var got int

	stderr := captureStderr(t, func() {
		got = run([]string{"init", filepath.Join(base, "a"), "--controlplane", "cp-01=10.0.0.11",
			"--talos-version", "v1.14.1"})
	})

	if got != 1 || !strings.Contains(stderr, "pass --talos-version and --kubernetes-version") {
		t.Errorf("exit %d\n%s\nwant it to name both version flags", got, stderr)
	}

	if got := run([]string{"init", filepath.Join(base, "b"), "--controlplane", "cp-01=10.0.0.11",
		"--talos-version", "v1.14.1", "--kubernetes-version", "v1.37.0"}); got != 0 {
		t.Errorf("init with both versions given exited %d without talosctl", got)
	}
}
