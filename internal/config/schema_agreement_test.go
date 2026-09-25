package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v4"
)

// TestSchemaAgreesWithLoad holds the published schema to what talman does: an
// editor using it must flag exactly the configs Load refuses. Each case is
// judged by both, and a disagreement either way fails -- a schema stricter
// than talman flags configs that work, and a looser one lets through configs
// that fail at the first command.
func TestSchemaAgreesWithLoad(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "talman.v1.json"))
	if err != nil {
		t.Fatal(err)
	}

	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaURL, doc); err != nil {
		t.Fatal(err)
	}

	schema, err := c.Compile(SchemaURL)
	if err != nil {
		t.Fatal(err)
	}

	const base = `apiVersion: talman.dev/v1
clusterName: c
endpoint: https://10.0.0.1:6443
talosVersion: v1.14.0
kubernetesVersion: v1.37.0
nodes:
  - hostname: cp-01
    ipAddress: 10.0.0.11
    role: controlplane
`

	// Each case is base with a change: extra top-level keys, extra keys on
	// the node, or a replacement.
	cases := map[string]struct {
		top, node   string
		from, to    string
		wantAccepts bool
	}{
		"base":                       {wantAccepts: true},
		"empty values":               {top: "values:\n", wantAccepts: true},
		"empty node values":          {node: "    values:\n", wantAccepts: true},
		"empty patches":              {top: "patches:\n", wantAccepts: true},
		"empty patches.all":          {top: "patches:\n  all:\n", wantAccepts: true},
		"empty imageFactory":         {top: "imageFactory:\n", wantAccepts: true},
		"empty valuesFiles":          {top: "valuesFiles:\n", wantAccepts: true},
		"empty rollout":              {top: "rollout:\n", wantAccepts: true},
		"empty schematic":            {top: "schematic:\n", wantAccepts: true},
		"empty outputDir":            {top: "outputDir:\n", wantAccepts: true},
		"empty customization":        {top: "schematic:\n  customization:\n", wantAccepts: true},
		"empty officialExtensions":   {top: "schematic:\n  customization:\n    systemExtensions:\n      officialExtensions:\n", wantAccepts: true},
		"empty overlay options":      {top: "schematic:\n  overlay:\n    name: x\n    options:\n", wantAccepts: true},
		"empty node groups":          {node: "    groups:\n", wantAccepts: true},
		"empty node imageFactory":    {node: "    imageFactory:\n", wantAccepts: true},
		"unquoted k8s version":       {from: "kubernetesVersion: v1.37.0", to: "kubernetesVersion: 1.37", wantAccepts: true},
		"talos version without v":    {from: "talosVersion: v1.14.0", to: "talosVersion: 1.14.0", wantAccepts: true},
		"numeric address":            {from: "ipAddress: 10.0.0.11", to: "ipAddress: 1011", wantAccepts: true},
		"bootloader in capitals":     {top: "schematic:\n  customization:\n    bootloader: GRUB\n", wantAccepts: true},
		"waves":                      {top: "rollout:\n  soak: 10m\n  waves: [controlplane]\n", wantAccepts: true},
		"no waves":                   {top: "rollout:\n  waves: []\n"},
		"an empty wave":              {top: "rollout:\n  waves:\n    - []\n"},
		"a wave as a mapping":        {top: "rollout:\n  waves:\n    - {groups: [controlplane]}\n"},
		"soak not a duration":        {top: "rollout:\n  soak: nope\n  waves: [controlplane]\n"},
		"soak a number":              {top: "rollout:\n  soak: 10\n  waves: [controlplane]\n"},
		"unknown validationMode":     {top: "validationMode: foo\n"},
		"schematic and schematicID":  {top: "schematicID: abc\nschematic:\n  customization: {}\n"},
		"node schematic and ID":      {node: "    schematicID: abc\n    schematic:\n      customization: {}\n"},
		"empty schematic path":       {top: "schematic: \"\"\n"},
		"secureBoot as a string":     {top: "imageFactory:\n  secureBoot: \"true\"\n"},
		"renamed secureboot":         {top: "imageFactory:\n  secureboot: true\n"},
		"renamed talosMode":          {top: "talosMode: metal\n"},
		"meta key out of range":      {top: "schematic:\n  customization:\n    meta:\n      - key: 300\n        value: x\n"},
		"unknown top-level key":      {top: "x-common: &c\n  a: 1\n"},
		"endpoint without a port":    {from: ":6443", to: ""},
		"no apiVersion":              {from: "apiVersion: talman.dev/v1\n", to: ""},
		"a later apiVersion":         {from: "apiVersion: talman.dev/v1", to: "apiVersion: talman.dev/v2"},
		"upper-case hostname":        {from: "hostname: cp-01", to: "hostname: CP-01"},
		"hostname with a slash":      {from: "hostname: cp-01", to: "hostname: a/b"},
		"cluster name with a slash":  {from: "clusterName: c", to: "clusterName: a/b"},
		"no nodes":                   {from: base[strings.Index(base, "nodes:"):], to: "nodes: []\n"},
		"an unknown role":            {from: "role: controlplane", to: "role: master"},
	}

	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			body := base
			if tt.from != "" {
				if !strings.Contains(body, tt.from) {
					t.Fatalf("case replaces %q, which base does not hold", tt.from)
				}

				body = strings.Replace(body, tt.from, tt.to, 1)
			}

			body += tt.node + tt.top

			_, loadErr := Load(write(t, body))
			talman := loadErr == nil

			var parsed any
			if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatal(err)
			}

			asJSON, err := json.Marshal(parsed)
			if err != nil {
				t.Fatal(err)
			}

			inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
			if err != nil {
				t.Fatal(err)
			}

			schemaErr := schema.Validate(inst)
			editor := schemaErr == nil

			switch {
			case talman != editor:
				t.Errorf("talman accepts: %t (%v)\nschema accepts: %t (%v)\n%s",
					talman, loadErr, editor, schemaErr, body)
			case talman != tt.wantAccepts:
				t.Errorf("both accept: %t, want %t (%v)", talman, tt.wantAccepts, loadErr)
			}
		})
	}
}
