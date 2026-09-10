package sopsx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// requireSops skips when the binary is absent. talman drives sops as a
// subprocess, so these tests exercise the real thing or nothing.
func requireSops(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath(Bin); err != nil {
		t.Skip("sops not on PATH")
	}
}

const secretBundle = `cluster:
    id: aWQ=
    secret: c2VjcmV0
secrets:
    bootstraptoken: abcdef.0123456789abcdef
    secretboxencryptionsecret: c2Vjcm V0Ym94
certs:
    os:
        crt: LS0tLS1CRUdJTg==
        key: LS0tLS1CRUdJTiBFRA==
`

// newKeyedDir returns a directory holding a .sops.yaml whose creation rule
// points at a freshly generated age recipient, and sets SOPS_AGE_KEY so the
// matching identity is available for decryption.
func newKeyedDir(t *testing.T, pathRegex string) string {
	t.Helper()

	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()

	conf := "creation_rules:\n  - path_regex: " + pathRegex +
		"\n    age: " + identity.Recipient().String() + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".sops.yaml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOPS_AGE_KEY", identity.String())

	return dir
}

// TestEncryptDecryptRoundTrip covers the path that decides whether an
// operator's cluster CAs land on disk encrypted or in the clear. Nothing
// exercised it before: the CI example step generated its bundle with
// talosctl directly, so EncryptTo never ran.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	requireSops(t)

	dir := newKeyedDir(t, `\.sops\.yaml$`)
	dest := filepath.Join(dir, "talsecret.sops.yaml")

	encrypted, err := EncryptTo([]byte(secretBundle), dest)
	if err != nil {
		t.Fatalf("EncryptTo: %v", err)
	}

	if !IsEncrypted(encrypted) {
		t.Error("EncryptTo produced output without SOPS metadata")
	}

	for _, plaintext := range []string{"abcdef.0123456789abcdef", "LS0tLS1CRUdJTiBFRA=="} {
		if strings.Contains(string(encrypted), plaintext) {
			t.Errorf("secret %q survives in the encrypted output", plaintext)
		}
	}

	if !strings.Contains(string(encrypted), "ENC[") {
		t.Error("no ENC[...] values in the encrypted output")
	}

	if err := os.WriteFile(dest, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(got) != secretBundle {
		t.Errorf("round trip changed the bundle:\n got %q\nwant %q", got, secretBundle)
	}
}

// Structure has to be preserved, not just content: talosctl parses the bundle
// by key, so encrypting the keys as well as the values would break it.
func TestEncryptionPreservesKeys(t *testing.T) {
	requireSops(t)

	dir := newKeyedDir(t, `\.sops\.yaml$`)

	encrypted, err := EncryptTo([]byte(secretBundle), filepath.Join(dir, "talsecret.sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"cluster:", "secrets:", "bootstraptoken:", "certs:", "os:"} {
		if !strings.Contains(string(encrypted), key) {
			t.Errorf("key %q was lost or encrypted; talosctl parses the bundle by key", key)
		}
	}
}

// An unencrypted file must pass through untouched, which is what lets a
// plaintext bundle work during bring-up.
func TestReadFilePassesPlaintextThrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.yaml")

	if err := os.WriteFile(path, []byte(secretBundle), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != secretBundle {
		t.Error("plaintext file was altered on read")
	}
}

func TestIsEncrypted(t *testing.T) {
	if IsEncrypted([]byte(secretBundle)) {
		t.Error("plaintext reported as encrypted")
	}

	if !IsEncrypted([]byte("a: ENC[x]\nsops:\n    version: 3.13.3\n")) {
		t.Error("SOPS metadata not detected")
	}

	if IsEncrypted([]byte("\tnot: yaml: at all\n")) {
		t.Error("unparseable input reported as encrypted")
	}
}

// Failing to find a rule must not silently write plaintext where an operator
// expects encryption.
func TestEncryptRefusesWithoutACreationRule(t *testing.T) {
	requireSops(t)

	dir := newKeyedDir(t, `this-will-never-match$`)

	_, err := EncryptTo([]byte(secretBundle), filepath.Join(dir, "talsecret.sops.yaml"))
	if err == nil {
		t.Fatal("EncryptTo succeeded with no matching creation rule")
	}

	if !strings.Contains(err.Error(), "no creation_rules entry matching") {
		t.Errorf("error should say the rule did not match, got: %v", err)
	}
}

// Decryption without the identity must fail loudly rather than yield
// ciphertext that later looks like a corrupt bundle.
func TestDecryptFailsWithoutTheKey(t *testing.T) {
	requireSops(t)

	dir := newKeyedDir(t, `\.sops\.yaml$`)
	dest := filepath.Join(dir, "talsecret.sops.yaml")

	encrypted, err := EncryptTo([]byte(secretBundle), dest)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dest, encrypted, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SOPS_AGE_KEY", "")
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(dir, "nonexistent"))

	if _, err := ReadFile(dest); err == nil {
		t.Fatal("ReadFile succeeded without the age identity")
	}
}

// A missing sops binary must be an explicit error. Reading an encrypted file
// as if it were plaintext would hand ciphertext to talosctl and produce a
// baffling parse failure instead.
func TestEncryptedFileWithoutSopsIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "talsecret.sops.yaml")

	if err := os.WriteFile(path, []byte("a: ENC[x]\nsops:\n    version: 3.13.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	old := Bin
	Bin = "sops-does-not-exist"

	defer func() { Bin = old }()

	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("ReadFile returned ciphertext as if it were plaintext")
	}

	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("error should be ErrNotInstalled, got: %v", err)
	}
}
