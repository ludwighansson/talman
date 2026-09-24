package cli

import (
	"os"
	"path/filepath"
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

	if exists(filepath.Join(bad, config.DefaultFileName)) {
		t.Error("a config that failed validation was left behind")
	}

	if got := run([]string{"init", bad, "--talos-version", "v1.14.1", "--kubernetes-version", "1.37.0"}); got != 1 {
		t.Errorf("init with no control plane gave exit %d, want 1", got)
	}
}
