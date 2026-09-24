// Package sopsx reads and writes SOPS-encrypted files by driving the sops
// binary.
//
// It shells out rather than embedding getsops/sops for the same reason talman
// shells out to talosctl: the library drags the AWS, GCP and Azure KMS SDKs in
// with it -- several hundred modules -- to support key services most clusters
// never use, and every one of them becomes talman's problem to keep patched.
// The cost is that `sops` has to be on PATH, which is checked for explicitly
// so a missing binary is a clear error rather than a file read as ciphertext.
package sopsx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/interrupt"
)

// Bin is the sops binary talman invokes. Overridable for tests.
var Bin = "sops"

// ErrNotInstalled is returned when the sops binary cannot be found.
var ErrNotInstalled = errors.New("sops not found on PATH")

// Ensure reports whether the sops binary is usable, with a message that says
// what to do about it.
func Ensure() error {
	if _, err := exec.LookPath(Bin); err != nil {
		return fmt.Errorf("%w: talman needs it to read and write encrypted secrets "+
			"(install sops, or keep the bundle unencrypted with --plaintext)", ErrNotInstalled)
	}

	return nil
}

// IsEncrypted reports whether data carries SOPS metadata.
//
// Done locally rather than via `sops filestatus` so that the common question
// "does this file even need decrypting" costs no subprocess and works when
// sops is absent.
func IsEncrypted(data []byte) bool {
	var probe struct {
		Sops any `yaml:"sops"`
	}

	if err := yaml.Unmarshal(data, &probe); err != nil {
		return false
	}

	return probe.Sops != nil
}

// ReadFile reads path, decrypting it if it is SOPS-encrypted and returning it
// verbatim if it is not. Plaintext is never written to disk.
func ReadFile(path string) ([]byte, error) {
	plaintext, _, err := Read(path)

	return plaintext, err
}

// Read is ReadFile, also returning the file as it is on disk when it was
// encrypted -- nil when it was not -- so a caller can tell which of the values
// were the encrypted ones.
func Read(path string) (plaintext, ciphertext []byte, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}

	if !IsEncrypted(data) {
		return data, nil, nil
	}

	plaintext, err = decrypt(path)
	if err != nil {
		return nil, nil, err
	}

	return plaintext, data, nil
}

func decrypt(path string) ([]byte, error) {
	if err := Ensure(); err != nil {
		return nil, fmt.Errorf("%s is SOPS-encrypted but %w", path, err)
	}

	plaintext, stderr, err := run("decrypt", "--input-type", "yaml", "--output-type", "yaml", path)
	if err != nil {
		return nil, fmt.Errorf("decrypting %s: %s", path, describe(stderr, err))
	}

	return plaintext, nil
}

// EncryptTo encrypts plaintext as YAML for the given destination path, using
// the creation rule .sops.yaml defines for that path. The path selects the
// rule; nothing is written to it here.
func EncryptTo(plaintext []byte, destPath string) ([]byte, error) {
	if err := Ensure(); err != nil {
		return nil, err
	}

	abs, err := filepath.Abs(destPath)
	if err != nil {
		return nil, err
	}

	// sops matches creation rules against the input file's path, so the
	// plaintext is staged under a temporary name and --filename-override
	// tells sops to match on the real destination instead. Passing the
	// plaintext on /dev/stdin would be shorter but is not portable.
	//
	// Staged in a private 0700 directory of its own, not beside the
	// destination: that is the operator's cluster repository, and a run
	// interrupted before the deferred removal would leave the plaintext CA
	// keys there for the next `git add .` to commit. The override is what
	// makes the location irrelevant to sops.
	dir := filepath.Dir(abs)

	stage, err := os.MkdirTemp("", "talman-secrets-")
	if err != nil {
		return nil, err
	}

	defer os.RemoveAll(stage) //nolint:errcheck // best effort cleanup; the error that matters is returned below
	defer interrupt.RemoveAllOnExit(stage)()

	tmpName := filepath.Join(stage, "secrets.yaml")

	if err := os.WriteFile(tmpName, plaintext, 0o600); err != nil {
		return nil, err
	}

	// The sops CLI resolves .sops.yaml relative to its own working directory,
	// not to the file being encrypted, so talman locates it and passes
	// --config explicitly. Otherwise whether a bundle gets encrypted would
	// depend on where the operator happened to be standing.
	conf, err := FindConfig(dir)
	if err != nil {
		return nil, err
	}

	// --config is a global flag, so it has to precede the subcommand.
	encrypted, stderr, err := run("--config", conf, "encrypt",
		"--input-type", "yaml", "--output-type", "yaml",
		"--filename-override", abs, tmpName)
	if err != nil {
		if strings.Contains(stderr, "no matching creation rules") {
			return nil, fmt.Errorf("%s has no creation_rules entry matching %s: "+
				"add one so talman knows which keys to encrypt to", conf, destPath)
		}

		return nil, fmt.Errorf("encrypting %s: %s", destPath, describe(stderr, err))
	}

	return encrypted, nil
}

// run invokes sops and returns stdout, sops' stderr, and the process error.
func run(args ...string) ([]byte, string, error) {
	cmd := interrupt.Command(Bin, args...)

	var out, errBuf bytes.Buffer

	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	err := cmd.Run()

	return out.Bytes(), errBuf.String(), err
}

// describe prefers sops' own message over the bare exit status.
func describe(stderr string, err error) string {
	if msg := strings.TrimSpace(stderr); msg != "" {
		return msg
	}

	return err.Error()
}

// FindConfig walks up from dir looking for a SOPS config, the way sops itself
// would from its working directory.
func FindConfig(dir string) (string, error) {
	start := dir

	for {
		for _, name := range []string{".sops.yaml", ".sops.yml"} {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .sops.yaml found in %s or any parent directory: "+
				"create one with a creation_rules entry, or write the secrets file "+
				"unencrypted with --plaintext", start)
		}

		dir = parent
	}
}
