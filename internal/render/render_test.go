package render

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// fixture lays out a two-node cluster with a shared patch tree one directory
// up, which is the layout talman exists to support.
//
// It generates the secrets bundle with talosctl, so it carries the skip for
// every test that uses it. On the caller it was a rule three tests forgot,
// which turned "talosctl is not installed" into three failures with nothing to
// do with what they test.
func fixture(t *testing.T) string {
	t.Helper()

	requireTalosctl(t)

	root := t.TempDir()

	files := map[string]string{
		"base/patches/all/install.yaml": `apiVersion: v1alpha1
kind: UnattendedInstallConfig
installer:
  image: {{ .Node.InstallerImage }}
provisioning:
  diskSelector:
    match: disk.dev_path == "{{ .Node.Values.installDisk }}"
  wipe: false
---
apiVersion: v1alpha1
kind: HostnameConfig
auto: "off"
hostname: {{ .Node.Hostname }}
`,
		"base/patches/all/cni.yaml": `apiVersion: v1alpha1
kind: KubeFlannelCNIConfig
$patch: delete
`,
		"cluster/patches/worker/kubelet.yaml": `apiVersion: v1alpha1
kind: KubeletConfig
extraArgs:
  rotate-server-certificates: "true"
`,
		"cluster/patches/db/sysctl.yaml": `apiVersion: v1alpha1
kind: SysctlConfig
params:
  vm.nr_hugepages: "{{ .Values.hugepages }}"
`,
		"cluster/patches/nodes/w1.yaml": `apiVersion: v1alpha1
kind: KubeNodeConfig
labels:
  special: "yes"
`,
		// A whole document behind a conditional: a normal idiom that must not
		// reach talosctl as an empty patch.
		"cluster/patches/all/maybe.yaml": `{{ if .Node.HasGroup "gpu" }}
apiVersion: v1alpha1
kind: SysctlConfig
params:
  x: "1"
{{ end }}`,
		"cluster/talman.yaml": `clusterName: testcluster
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
secretFile: secrets.yaml

values:
  hugepages: "1024"

imageFactory:
  installerURLTmpl: "{{.RegistryURL}}/openstack-installer/{{.ID}}:{{.Version}}"

schematic:
  customization:
    systemExtensions:
      officialExtensions: [siderolabs/drbd]

patches:
  all:
    - ../base/patches/all/cni.yaml
    - ../base/patches/all/install.yaml
    - ./patches/all/maybe.yaml
  worker:
    - ./patches/worker/kubelet.yaml
  db:
    - ./patches/db/sysctl.yaml

nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
    values:
      installDisk: /dev/vda
  - hostname: w1
    ipAddress: 10.0.0.11
    role: worker
    groups: [db]
    values:
      installDisk: /dev/vdb
    patches:
      - ./patches/nodes/w1.yaml
`,
	}

	for rel, body := range files {
		path := filepath.Join(root, rel)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// An unencrypted bundle: sopsx passes plaintext through, so the render
	// path is exercised without needing an age key in CI.
	tal := talosctl.New("talosctl")

	bundle, err := tal.GenSecrets(talosctl.GenSecretsOptions{TalosVersion: "v1.14.0"})
	if err != nil {
		t.Fatalf("talosctl gen secrets: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "cluster", "secrets.yaml"), bundle, 0o600); err != nil {
		t.Fatal(err)
	}

	return filepath.Join(root, "cluster", "talman.yaml")
}

func requireTalosctl(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("talosctl"); err != nil {
		t.Skip("talosctl not on PATH")
	}
}

func TestRenderProducesTargetedConfigs(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}

	if err := r.Open(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	out := map[string]string{}

	for i := range cfg.Nodes {
		res, err := r.Node(&cfg.Nodes[i])
		if err != nil {
			t.Fatalf("node %s: %v", cfg.Nodes[i].Hostname, err)
		}

		// Everything talman emits must survive Talos' own validation.
		if err := r.Validate(res, cfg.TalosMode); err != nil {
			t.Errorf("rendered config for %s is invalid: %v", cfg.Nodes[i].Hostname, err)
		}

		out[cfg.Nodes[i].Hostname] = string(res.Content)
	}

	cp, w := out["c1"], out["w1"]

	t.Run("templating resolved per node", func(t *testing.T) {
		mustContain(t, cp, `hostname: c1`)
		mustContain(t, w, `hostname: w1`)
		mustContain(t, cp, `disk.dev_path == "/dev/vda"`)
		mustContain(t, w, `disk.dev_path == "/dev/vdb"`)
	})

	t.Run("installer image comes from the schematic", func(t *testing.T) {
		mustContain(t, w, "factory.talos.dev/openstack-installer/")
		// Talos v1.14 carries the installer in UnattendedInstallConfig, not
		// the deprecated .machine.install.
		mustContain(t, w, "kind: UnattendedInstallConfig")
	})

	t.Run("role patches apply only to that role", func(t *testing.T) {
		mustContain(t, w, "rotate-server-certificates")
		mustNotContain(t, cp, "rotate-server-certificates")
	})

	t.Run("group patches apply only to members", func(t *testing.T) {
		mustContain(t, w, "vm.nr_hugepages")
		mustContain(t, w, `"1024"`)
		mustNotContain(t, cp, "vm.nr_hugepages")
	})

	t.Run("node patches apply only to that node", func(t *testing.T) {
		mustContain(t, w, "special:")
		mustNotContain(t, cp, "special:")
	})

	t.Run("patch delete removes a generated document", func(t *testing.T) {
		mustNotContain(t, cp, "KubeFlannelCNIConfig")
		mustNotContain(t, w, "KubeFlannelCNIConfig")
	})

	t.Run("output type follows the role", func(t *testing.T) {
		mustContain(t, cp, "type: controlplane")
		mustContain(t, w, "type: worker")
	})
}

// TestBlameNamesTheOffendingPatch covers the failure path: talosctl reports
// the offending document but never the file, so talman has to find it.
func TestBlameNamesTheOffendingPatch(t *testing.T) {
	path := fixture(t)

	bad := filepath.Join(filepath.Dir(path), "patches", "db", "sysctl.yaml")
	if err := os.WriteFile(bad, []byte("apiVersion: v1alpha1\nkind: SysctlConfig\nsysctls:\n  a: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}

	if err := r.Open(); err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// c1 is not in the db group, so it must still render.
	if _, err := r.Node(&cfg.Nodes[0]); err != nil {
		t.Errorf("control plane should be unaffected: %v", err)
	}

	_, err = r.Node(&cfg.Nodes[1])
	if err == nil {
		t.Fatal("expected the worker render to fail")
	}

	if !strings.Contains(err.Error(), "patches.db") ||
		!strings.Contains(err.Error(), "sysctl.yaml") {
		t.Errorf("error does not name the offending patch:\n%v", err)
	}
}

func TestOpenRequiresSecrets(t *testing.T) {
	path := fixture(t)

	if err := os.Remove(filepath.Join(filepath.Dir(path), "secrets.yaml")); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}

	err = r.Open()
	if err == nil {
		r.Close()
		t.Fatal("expected an error when the secrets bundle is missing")
	}

	if !strings.Contains(err.Error(), "secrets generate") {
		t.Errorf("error does not point at the fix: %v", err)
	}
}

// The decrypted bundle lives in a temp dir for the duration of a pass and must
// not survive it.
func TestCloseRemovesDecryptedSecrets(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}

	if err := r.Open(); err != nil {
		t.Fatal(err)
	}

	staged := r.secretsFile

	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("expected a staged bundle at %s: %v", staged, err)
	}

	r.Close()

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("%s still exists after Close", staged)
	}
}

func TestNodesSelection(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	all, err := Nodes(cfg, nil)
	if err != nil || len(all) != 2 {
		t.Fatalf("Nodes(nil) = %d nodes, %v; want 2", len(all), err)
	}

	byName, err := Nodes(cfg, []string{"w1"})
	if err != nil || len(byName) != 1 || byName[0].Hostname != "w1" {
		t.Fatalf("selection by hostname failed: %v", err)
	}

	byIP, err := Nodes(cfg, []string{"10.0.0.10"})
	if err != nil || byIP[0].Hostname != "c1" {
		t.Fatalf("selection by address failed: %v", err)
	}

	_, err = Nodes(cfg, []string{"nope"})
	if err == nil || !strings.Contains(err.Error(), "known nodes are") {
		t.Errorf("unknown node error should list the known ones, got: %v", err)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()

	if !strings.Contains(haystack, needle) {
		t.Errorf("output does not contain %q", needle)
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()

	if strings.Contains(haystack, needle) {
		t.Errorf("output unexpectedly contains %q", needle)
	}
}

// TestNodesDeduplicates covers a selection reaching the destructive commands
// twice. `-n w1 -n 10.0.0.11` names one machine two ways, and apply, upgrade
// and reset all consume this slice -- a duplicate there means resetting the
// same node twice off a single confirmation that counted it twice.
func TestNodesDeduplicates(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, names := range [][]string{
		{"w1", "w1"},
		{"w1", "10.0.0.11"},
		{"10.0.0.11", "w1", "w1"},
	} {
		got, err := Nodes(cfg, names)
		if err != nil {
			t.Fatalf("Nodes(%v): %v", names, err)
		}

		if len(got) != 1 || got[0].Hostname != "w1" {
			t.Errorf("Nodes(%v) returned %d nodes, want exactly one (w1)", names, len(got))
		}
	}

	// Distinct nodes must still both survive.
	both, err := Nodes(cfg, []string{"c1", "w1"})
	if err != nil || len(both) != 2 {
		t.Errorf("Nodes(c1,w1) = %d nodes, %v; want 2", len(both), err)
	}
}

// TestNodeRequiresOpen: without Open the workspace is "", and the patch
// directory would resolve against the caller's working directory -- writing
// rendered patches, possibly holding secrets decrypted out of SOPS, into the
// operator's repo. Context() is deliberately usable un-Opened, so Node() has
// to refuse on its own.
func TestNodeRequiresOpen(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.Node(&cfg.Nodes[0]); err == nil {
		t.Fatal("Node succeeded without Open; it should refuse")
	}

	if _, err := os.Stat(filepath.Join(wd, "patches")); !os.IsNotExist(err) {
		t.Errorf("Node created a patches/ directory in the working directory")
	}
}

// TestGitignoreRestoredWhenIneffective: the file is only protection if it
// actually ignores. A truncated one leaves machine configs -- which carry the
// machine CA key and bootstrap token -- stageable.
func TestGitignoreRestoredWhenIneffective(t *testing.T) {
	cfg, err := config.Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}

	dir := cfg.OutputPath()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	gitignore := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte("# emptied by something\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Renderer{Cfg: cfg, Tal: talosctl.New(cfg.Talosctl)}
	if err := r.ensureGitignore(dir); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(body), "\n*\n") {
		t.Errorf("ineffective .gitignore was left in place:\n%s", body)
	}
}

// TestWriteAtomicReplacesInPlace: os.WriteFile truncates first, so an
// interrupted write used to leave a half-written machine config, or a
// talosconfig with no endpoints, where a working one had been.
func TestWriteAtomicReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.yaml")

	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeAtomic(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "replacement" {
		t.Errorf("content = %q, want %q", got, "replacement")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: rendered configs contain cluster secrets", perm)
	}

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the target", len(entries))
	}
}
