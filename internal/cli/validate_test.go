package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/ludwighansson/talman/internal/sopsx"
)

// TestValidateEncryptedPatches: validate needs no keys, so a patch it cannot
// decrypt for want of one is noted and passes -- but a patch the key opens
// and sops cannot, because the file is damaged, fails it.
func TestValidateEncryptedPatches(t *testing.T) {
	if _, err := exec.LookPath(sopsx.Bin); err != nil {
		t.Skip("sops not on PATH")
	}

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	patch := filepath.Join(dir, "patches", "secret.sops.yaml")

	files := map[string]string{
		".sops.yaml": "creation_rules:\n  - path_regex: \\.sops\\.yaml$\n    age: " +
			identity.Recipient().String() + "\n",
		"talman.yaml": `apiVersion: talman.dev/v1
clusterName: v
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.1
kubernetesVersion: v1.37.0
patches:
  all: [./patches/secret.sops.yaml]
nodes:
  - hostname: c1
    ipAddress: 10.0.0.1
    role: controlplane
  - hostname: c2
    ipAddress: 10.0.0.2
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.3
    role: worker
`,
	}

	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(patch), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOPS_AGE_KEY", identity.String())

	encrypted, err := sopsx.EncryptTo([]byte("machine:\n  network:\n    hostname: secret\n"), patch)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(patch, encrypted, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TALMAN_CONFIG", filepath.Join(dir, "talman.yaml"))

	// Counted through a wrapper: one decryption per file, however many
	// nodes the patch reaches.
	counter := filepath.Join(dir, "sops-calls")
	wrapper := filepath.Join(dir, "sops-counting")

	sops, err := exec.LookPath(sopsx.Bin)
	if err != nil {
		t.Fatal(err)
	}

	script := "#!/bin/sh\necho x >> " + counter + "\nexec " + sops + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	saved := sopsx.Bin
	sopsx.Bin = wrapper

	t.Cleanup(func() { sopsx.Bin = saved })

	if got := run([]string{"validate"}); got != 0 {
		t.Errorf("with the key: exit %d", got)
	}

	if calls, _ := os.ReadFile(counter); strings.Count(string(calls), "x") != 1 {
		t.Errorf("sops ran %d times for one patch on three nodes, want 1", strings.Count(string(calls), "x"))
	}

	t.Setenv("SOPS_AGE_KEY", "")
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(dir, "no-such-key"))

	if got := run([]string{"validate"}); got != 0 {
		t.Errorf("without the key: exit %d, want 0 with a note", got)
	}

	t.Setenv("SOPS_AGE_KEY", identity.String())

	damaged := regexp.MustCompile(`hostname: ENC\[AES256_GCM,data:[^,]*`).
		ReplaceAll(encrypted, []byte("hostname: ENC[AES256_GCM,data:AAAA"))
	if err := os.WriteFile(patch, damaged, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := run([]string{"validate"}); got != 1 {
		t.Errorf("a damaged patch: exit %d, want 1", got)
	}
}

// Several problems print as a list under their count, each problem's later
// lines under it, and a problem every node shares is listed once.
func TestValidateListsProblemsOnce(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"all.yaml": "machine:\n  network:\n    hostname: {{ .Node.Values.zone }}\n",
		"db.yaml":  "machine:\n\tsysctls: {}\n",
		"talman.yaml": `apiVersion: talman.dev/v1
clusterName: v
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.1
kubernetesVersion: v1.37.0
patches:
  all: [./all.yaml]
  db: [./db.yaml]
nodes:
  - hostname: c1
    ipAddress: 10.0.0.1
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
    groups: [db]
  - hostname: w2
    ipAddress: 10.0.0.3
    role: worker
`,
	}

	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("TALMAN_CONFIG", filepath.Join(dir, "talman.yaml"))

	stderr := captureStderr(t, func() {
		if got := run([]string{"validate"}); got != 1 {
			t.Errorf("exit %d, want 1", got)
		}
	})

	for _, want := range []string{
		"error: 2 problems:\n  all 3 nodes: patches.all: ./all.yaml",
		"\n      a key that may be missing is read with index",
		"\n  node w1: patches.db: ./db.yaml: document 1 is not valid YAML",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("validate does not say %q:\n%s", want, stderr)
		}
	}

	if n := strings.Count(stderr, "./all.yaml"); n != 1 {
		t.Errorf("a problem every node shares was listed %d times:\n%s", n, stderr)
	}
}

// One problem is one line, as any error is; two or more are a list.
func TestErrorText(t *testing.T) {
	one := errors.Join(errors.New("only"))
	if got := errorText(one); got != "error: only" {
		t.Errorf("errorText(one) = %q", got)
	}

	two := errors.Join(errors.New("first\n  its hint"), errors.New("second"))
	if got, want := errorText(two), "error: 2 problems:\n  first\n      its hint\n  second"; got != want {
		t.Errorf("errorText(two) = %q, want %q", got, want)
	}

	wrapped := fmt.Errorf("context: %w", two)
	if got := errorText(wrapped); !strings.HasPrefix(got, "error: context: first") {
		t.Errorf("a wrapped join is the wrapper's to word: %q", got)
	}
}
