package render

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
)

// TestWriteAllGuardsSecrets holds the output directory to what the README
// promises: configs 0600 in a 0700 directory, behind a .gitignore that
// ignores everything, and an operator's own .gitignore that already ignores
// everything left alone.
func TestWriteAllGuardsSecrets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}

	dir := t.TempDir()
	cfg := &config.Config{Dir: dir, OutputDir: "clusterconfig"}
	node := &config.Node{Hostname: "c1"}
	r := &Renderer{Cfg: cfg}

	res := &Result{Node: node, Path: cfg.MachineConfigPath(node), Content: []byte("secret: yes\n")}

	if err := r.WriteAll([]*Result{res}, false); err != nil {
		t.Fatal(err)
	}

	mode := func(path string) os.FileMode {
		t.Helper()

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}

		return info.Mode().Perm()
	}

	out := cfg.OutputPath()

	if got := mode(out); got != 0o700 {
		t.Errorf("output directory is %o, want 700", got)
	}

	if got := mode(res.Path); got != 0o600 {
		t.Errorf("rendered config is %o, want 600", got)
	}

	ignore, err := os.ReadFile(filepath.Join(out, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}

	if !ignoresEverything(ignore) {
		t.Errorf(".gitignore does not ignore the whole directory:\n%s", ignore)
	}

	// Written again over a file that is already there: atomically, and still
	// 0600 however the previous one was left.
	if err := os.Chmod(res.Path, 0o644); err != nil {
		t.Fatal(err)
	}

	res.Content = []byte("secret: rotated\n")

	if err := r.WriteAll([]*Result{res}, false); err != nil {
		t.Fatal(err)
	}

	if got := mode(res.Path); got != 0o600 {
		t.Errorf("rewritten config is %o, want 600", got)
	}

	if got, _ := os.ReadFile(res.Path); string(got) != "secret: rotated\n" {
		t.Errorf("rewritten config holds %q", got)
	}

	leftovers, _ := filepath.Glob(filepath.Join(out, ".*.tmp*"))
	if len(leftovers) > 0 {
		t.Errorf("temp files left in the output directory: %v", leftovers)
	}
}
