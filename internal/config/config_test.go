package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write lays out a config plus patch files in a temp dir and returns the
// config path.
func write(t *testing.T, body string, patchFiles ...string) string {
	t.Helper()

	dir := t.TempDir()

	for _, rel := range patchFiles {
		path := filepath.Join(dir, rel)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("machine: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, DefaultFileName)

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

const validBase = `apiVersion: talman.dev/v1
clusterName: test
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
`

func TestPatchChainOrder(t *testing.T) {
	// Order is the contract: Talos applies strategic merge patches in
	// sequence and the last writer wins, so all -> role -> groups -> node is
	// what makes "the node's own patch overrides everything" true.
	path := write(t, validBase+`
patches:
  all: [p/all.yaml]
  worker: [p/worker.yaml]
  controlplane: [p/cp.yaml]
  db: [p/db.yaml]
  storage: [p/storage.yaml]
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: worker
    groups: [db, storage]
    patches: [p/w1.yaml]
  - hostname: c1
    ipAddress: 10.0.0.11
    role: controlplane
`, "p/all.yaml", "p/worker.yaml", "p/cp.yaml", "p/db.yaml", "p/storage.yaml", "p/w1.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	chain := cfg.PatchChain(&cfg.Nodes[0])

	var got []string
	for _, ref := range chain {
		got = append(got, ref.Group+":"+ref.Rel)
	}

	want := []string{
		"all:p/all.yaml",
		"worker:p/worker.yaml",
		"db:p/db.yaml",
		"storage:p/storage.yaml",
		"node:p/w1.yaml",
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("chain =\n  %v\nwant\n  %v", got, want)
	}
}

func TestPatchChainGroupsFollowNodeDeclarationOrder(t *testing.T) {
	// Two nodes in the same groups, declared in opposite orders, must get
	// their patches in their own declared order -- group precedence is the
	// node's to decide, not the map's.
	path := write(t, validBase+`
patches:
  db: [p/db.yaml]
  storage: [p/storage.yaml]
nodes:
  - hostname: a
    ipAddress: 10.0.0.10
    role: controlplane
    groups: [db, storage]
  - hostname: b
    ipAddress: 10.0.0.11
    role: worker
    groups: [storage, db]
`, "p/db.yaml", "p/storage.yaml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	first := cfg.PatchChain(&cfg.Nodes[0])
	if first[0].Group != "db" || first[1].Group != "storage" {
		t.Errorf("node a: got %s,%s want db,storage", first[0].Group, first[1].Group)
	}

	second := cfg.PatchChain(&cfg.Nodes[1])
	if second[0].Group != "storage" || second[1].Group != "db" {
		t.Errorf("node b: got %s,%s want storage,db", second[0].Group, second[1].Group)
	}
}

func TestPatchPathsResolveAgainstConfigDir(t *testing.T) {
	// A shared patch tree next to the cluster directory is the layout this
	// tool exists to support, so ../ must work.
	dir := t.TempDir()

	base := filepath.Join(dir, "base", "patches", "all")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(base, "cni.yaml"), []byte("machine: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cluster := filepath.Join(dir, "cluster")
	if err := os.MkdirAll(cluster, 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(cluster, DefaultFileName)
	body := validBase + `
patches:
  all: ["../base/patches/all/cni.yaml"]
nodes:
  - hostname: cp1
    ipAddress: 10.0.0.10
    role: controlplane
`

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	got := cfg.PatchChain(&cfg.Nodes[0])[0].Path
	want := filepath.Join(dir, "base", "patches", "all", "cni.yaml")

	if got != want {
		t.Errorf("resolved to %s, want %s", got, want)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		files   []string
		wantErr string
	}{
		{
			name: "patches key naming no declared group",
			body: validBase + `
patches:
  db: [p/db.yaml]
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: worker
`,
			files:   []string{"p/db.yaml"},
			wantErr: `no node declares the group "db"`,
		},
		{
			name: "patch file missing",
			body: validBase + `
patches:
  all: [p/nope.yaml]
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "does not exist",
		},
		{
			name: "patch path is a directory",
			body: validBase + `
patches:
  all: [p]
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			files:   []string{"p/x.yaml"},
			wantErr: "is a directory",
		},
		{
			name: "invalid role",
			body: validBase + `
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: controller
`,
			wantErr: `invalid role "controller"`,
		},
		{
			name: "reserved group name on a node",
			body: validBase + `
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: worker
    groups: [all]
  - hostname: c1
    ipAddress: 10.0.0.11
    role: controlplane
`,
			wantErr: `group "all" is reserved`,
		},
		{
			name: "no control plane",
			body: validBase + `
nodes:
  - hostname: w1
    ipAddress: 10.0.0.10
    role: worker
`,
			wantErr: "at least one node must have role: controlplane",
		},
		{
			name: "duplicate hostname",
			body: validBase + `
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
  - hostname: c1
    ipAddress: 10.0.0.11
    role: worker
`,
			wantErr: "duplicate hostname",
		},
		{
			name: "duplicate address",
			body: validBase + `
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.10
    role: worker
`,
			wantErr: "duplicate ipAddress",
		},
		{
			// It names the default etcd snapshot, among other files.
			name: "cluster name that is a path",
			body: `apiVersion: talman.dev/v1
clusterName: a/../../x
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "clusterName",
		},
		{
			name: "endpoint without a port",
			body: `apiVersion: talman.dev/v1
clusterName: t
endpoint: https://10.0.0.1
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "including the port",
		},
		{
			// The hostname is the rendered file's name: a path in it would
			// write a config full of secrets outside the output directory.
			name: "hostname that is a path",
			body: validBase + `
nodes:
  - hostname: ../../escaped
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "not a valid RFC 1123 host name",
		},
		{
			name: "upper-case hostname",
			body: validBase + `
nodes:
  - hostname: Control-01
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "not a valid RFC 1123 host name",
		},
		{
			name: "group listed twice",
			body: validBase + `
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
    groups: [db, db]
`,
			wantErr: "listed twice",
		},
		{
			name: "unknown validation mode",
			body: validBase + `validationMode: bare
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "validationMode",
		},
		{
			name: "two documents",
			body: validBase + `nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
---
clusterName: other
`,
			wantErr: "more than one YAML document",
		},
		{
			name: "a prerelease key",
			body: validBase + `talosMode: metal
imageFactory:
  secureboot: true
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "renamed secureBoot",
		},
		{
			name:    "empty file",
			body:    "",
			wantErr: "is empty",
		},
		{
			// A later schema will have keys this one lacks; the refusal
			// has to name the schema, not the first unknown key.
			name: "a later schema with keys this one lacks",
			body: `apiVersion: talman.dev/v2
cluster:
  name: t
`,
			wantErr: "not one this talman understands",
		},
		{
			name: "unknown top level field",
			body: validBase + `
patchez:
  all: []
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "patchez",
		},
		{
			name: "missing talosVersion",
			body: `apiVersion: talman.dev/v1
clusterName: t
endpoint: https://10.0.0.1:6443
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "talosVersion is required",
		},
		{
			name: "endpoint without a scheme",
			body: `apiVersion: talman.dev/v1
clusterName: t
endpoint: 10.0.0.1:6443
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "must be a full https URL",
		},
		{
			name: "schematic and schematicID together",
			body: validBase + `
schematicID: abc
schematic:
  customization: {}
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`,
			wantErr: "mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.body, tt.files...))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error was %q,\nwant it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	// Fixing one path per run is a poor trade for a config that is mostly a
	// list of paths.
	_, err := Load(write(t, validBase+`
patches:
  all: [p/a.yaml, p/b.yaml]
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`))
	if err == nil {
		t.Fatal("expected errors")
	}

	if got := strings.Count(err.Error(), "does not exist"); got != 2 {
		t.Errorf("reported %d missing files, want 2:\n%s", got, err)
	}
}

func TestTalosVersionNormalised(t *testing.T) {
	cfg, err := Load(write(t, `apiVersion: talman.dev/v1
clusterName: t
endpoint: https://10.0.0.1:6443
talosVersion: "1.14.0"
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
    talosVersion: "1.13.5"
`))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.TalosVersion != "v1.14.0" {
		t.Errorf("cluster talosVersion = %q, want v1.14.0", cfg.TalosVersion)
	}

	if got := cfg.Nodes[0].EffectiveTalosVersion(cfg); got != "v1.13.5" {
		t.Errorf("node talosVersion = %q, want v1.13.5", got)
	}
}

func TestNodeLookupByHostnameThenAddress(t *testing.T) {
	cfg, err := Load(write(t, validBase+`
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.11
    role: worker
`))
	if err != nil {
		t.Fatal(err)
	}

	if n, ok := cfg.Node("w1"); !ok || n.IPAddress != "10.0.0.11" {
		t.Error("lookup by hostname failed")
	}

	if n, ok := cfg.Node("10.0.0.10"); !ok || n.Hostname != "c1" {
		t.Error("lookup by address failed")
	}

	if _, ok := cfg.Node("nope"); ok {
		t.Error("lookup of an unknown name succeeded")
	}
}

func TestInlineAndFileSchematics(t *testing.T) {
	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte("customization: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, DefaultFileName)
	body := validBase + `
schematic: ./s.yaml
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
    schematic:
      customization:
        systemExtensions:
          officialExtensions: [siderolabs/drbd]
`

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Schematic == nil || cfg.Schematic.Path != "./s.yaml" {
		t.Errorf("cluster schematic path = %+v, want ./s.yaml", cfg.Schematic)
	}

	inline := cfg.Nodes[0].Schematic
	if inline == nil || inline.Inline == nil {
		t.Fatal("node schematic did not parse as inline")
	}

	if got := inline.Inline.Customization.SystemExtensions.OfficialExtensions; len(got) != 1 {
		t.Errorf("inline extensions = %v, want one entry", got)
	}
}

// TestPatchPathOrderIsDeterministic checks that validation lists
// every problem in one pass: Go randomises map iteration, so without an
// explicit sort the same broken config reports its errors in a different
// order on every run and cannot be diffed or worked through top to bottom.
func TestPatchPathOrderIsDeterministic(t *testing.T) {
	body := validBase + `
patches:
  all: [p/a.yaml]
  worker: [p/w.yaml]
  db: [p/d.yaml]
  storage: [p/s.yaml]
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.11
    role: worker
    groups: [db, storage]
`
	cfg, err := LoadNoValidate(write(t, body, "p/a.yaml", "p/w.yaml", "p/d.yaml", "p/s.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var first []string

	for run := range 20 {
		var got []string
		for _, ref := range cfg.AllPatchPaths() {
			got = append(got, ref.Group+":"+ref.Rel)
		}

		if run == 0 {
			first = got

			continue
		}

		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("AllPatchPaths order varies between runs:\n  %v\n  %v", first, got)
		}
	}
}

// The field exists so a later, incompatible schema can be told apart from
// this one. A config written before it existed still has to work.
func TestAPIVersion(t *testing.T) {
	base := `clusterName: dev
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.1
kubernetesVersion: v1.37.0
nodes:
  - hostname: c1
    ipAddress: 10.0.0.11
    role: controlplane
`

	tests := []struct {
		name    string
		header  string
		wantErr string
	}{
		{
			// Required from v1 on: a config that does not say which schema
			// it was written against is exactly the one a later talman
			// would have to guess about.
			name:    "absent",
			header:  "",
			wantErr: "apiVersion is required",
		},
		{
			name:   "the schema talman speaks",
			header: "apiVersion: " + APIVersion + "\n",
		},
		{
			// Refused rather than read hopefully: the fields talman
			// recognises may mean something else in another schema, and
			// guessing at a machine configuration is how a cluster gets a
			// setting nobody wrote.
			name:    "a schema from the future",
			header:  "apiVersion: talman.dev/v2\n",
			wantErr: "not one this talman understands",
		},
		{
			name:    "something else entirely",
			header:  "apiVersion: v1\n",
			wantErr: "not one this talman understands",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "talman.yaml")

			if err := os.WriteFile(path, []byte(tt.header+base), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := Load(path)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Load() = %v, want no error", err)
			case tt.wantErr != "" && err == nil:
				t.Fatal("Load() succeeded, want an error naming the schema")
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("error does not say which schema is wrong: %v", err)
			}
		})
	}
}

func TestHostnames(t *testing.T) {
	for h, want := range map[string]bool{
		"c1":                            true,
		"talos-w01":                     true,
		"w01.dc1.example.net":           true,
		"":                              false,
		"-w01":                          false,
		"w01-":                          false,
		"W01":                           false,
		"w_01":                          false,
		"a/b":                           false,
		"..":                            false,
		"a..b":                          false,
		strings.Repeat("a", 64):         false,
		strings.Repeat("a.", 127) + "a": false,
	} {
		if got := validHostname(h); got != want {
			t.Errorf("validHostname(%q) = %t, want %t", h, got, want)
		}
	}
}

func TestPerNodeImageFactory(t *testing.T) {
	cfg, err := Load(write(t, validBase+`imageFactory:
  platform: openstack
  secureBoot: true
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
  - hostname: w1
    ipAddress: 10.0.0.11
    role: worker
    imageFactory:
      platform: metal
      secureBoot: false
`))
	if err != nil {
		t.Fatal(err)
	}

	c1, w1 := &cfg.Nodes[0], &cfg.Nodes[1]

	if got := cfg.ImageFactoryFor(c1); got.Platform != "openstack" || !got.SecureBootEnabled() {
		t.Errorf("c1 inherits %+v, want the cluster's openstack with secure boot", got)
	}

	if got := cfg.ImageFactoryFor(w1); got.Platform != "metal" || got.SecureBootEnabled() {
		t.Errorf("w1 gets %+v, want its own metal without secure boot", got)
	}

	if got := cfg.ImageFactoryFor(w1).RegistryURL; got != "factory.talos.dev" {
		t.Errorf("w1 registry = %q, want the default carried through", got)
	}

	if got := cfg.ValidationModeFor(c1); got != "cloud" {
		t.Errorf("validation mode for an openstack node = %q, want cloud", got)
	}

	if got := cfg.ValidationModeFor(w1); got != "metal" {
		t.Errorf("validation mode for a metal node = %q, want metal", got)
	}
}

func TestFindConfig(t *testing.T) {
	t.Setenv(EnvConfig, "")

	if got := FindConfig(""); got != DefaultFileName {
		t.Errorf("FindConfig(\"\") = %q, want %q", got, DefaultFileName)
	}

	t.Setenv(EnvConfig, "clusters/prod/talman.yaml")

	if got := FindConfig(""); got != "clusters/prod/talman.yaml" {
		t.Errorf("FindConfig(\"\") = %q, want $%s", got, EnvConfig)
	}

	if got := FindConfig("explicit.yaml"); got != "explicit.yaml" {
		t.Errorf("FindConfig(explicit) = %q; -c must win over the environment", got)
	}
}

func TestValuesFiles(t *testing.T) {
	path := write(t, validBase+`valuesFiles: [shared.yaml, site.yaml]
values:
  registry:
    mirror: inline.example
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
    valuesFiles: [node.yaml]
    values:
      zone: az2
`)
	dir := filepath.Dir(path)

	for name, body := range map[string]string{
		"shared.yaml": "registry:\n  mirror: shared.example\n  insecure: false\nntp: [a, b]\n",
		"site.yaml":   "registry:\n  insecure: true\nntp: [c]\n",
		"node.yaml":   "zone: az1\ndisk: /dev/vda\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	registry, _ := cfg.Values["registry"].(map[string]any)

	switch {
	case registry["mirror"] != "inline.example":
		t.Errorf("registry.mirror = %v; the inline map must win", registry["mirror"])
	case registry["insecure"] != true:
		t.Errorf("registry.insecure = %v; a later file must win over an earlier one, key by key", registry["insecure"])
	case fmt.Sprint(cfg.Values["ntp"]) != "[c]":
		t.Errorf("ntp = %v; a list is replaced, not appended to", cfg.Values["ntp"])
	}

	if n := cfg.Nodes[0].Values; n["zone"] != "az2" || n["disk"] != "/dev/vda" {
		t.Errorf("node values = %v", n)
	}

	if err := os.WriteFile(filepath.Join(dir, "site.yaml"), []byte("a: ENC[x]\nsops:\n  version: 3.9.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "SOPS-encrypted") {
		t.Errorf("an encrypted values file gave %v, want it refused", err)
	}

	if err := os.Remove(filepath.Join(dir, "node.yaml")); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing values file gave %v", err)
	}
}

// A trailing separator, or a trailing document holding only comments, is not
// a second config: plenty of generated YAML ends that way.
func TestTrailingEmptyDocument(t *testing.T) {
	body := validBase + `nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`

	for name, tail := range map[string]string{
		"separator":          "---\n",
		"separator, comment": "---\n# nothing here\n",
		"two separators":     "---\n---\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(write(t, body+tail)); err != nil {
				t.Errorf("Load() = %v", err)
			}
		})
	}
}

// A mapping with a non-string key decodes as map[any]any, and has to merge
// key by key like any other.
func TestValuesFilesMergeNonStringKeys(t *testing.T) {
	path := write(t, validBase+`valuesFiles: [shared.yaml]
values:
  ports: {8080: alt}
nodes:
  - hostname: c1
    ipAddress: 10.0.0.10
    role: controlplane
`)

	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "shared.yaml"),
		[]byte("ports: {80: http, 443: https}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	ports, ok := cfg.Values["ports"].(map[any]any)
	if !ok || ports[80] != "http" || ports[443] != "https" || ports[8080] != "alt" {
		t.Errorf("ports = %#v, want all three, keyed as YAML wrote them", cfg.Values["ports"])
	}

	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "shared.yaml"),
		[]byte("a: 1\n---\nb: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("a values file with two documents gave %v", err)
	}
}
