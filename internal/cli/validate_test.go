package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

	if got := run([]string{"validate"}); got != 0 {
		t.Errorf("with the key: exit %d", got)
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
