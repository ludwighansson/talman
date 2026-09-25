package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/ludwighansson/talman/internal/factory"
)

// SchemaURL is where the JSON schema for talman.yaml is published, for an
// editor to validate and complete against:
//
//	# yaml-language-server: $schema=https://raw.githubusercontent.com/ludwighansson/talman/main/schema/talman.v1.json
//
// It describes talman.dev/v1, which only ever gains keys, so the copy on main
// is right for every v1 config.
const SchemaURL = "https://raw.githubusercontent.com/ludwighansson/talman/main/schema/talman.v1.json"

// JSONSchema describes talman.yaml as a JSON schema, generated from the types
// Load decodes into so that the two cannot drift apart: a key the schema
// accepts is one talman accepts, and additionalProperties is false wherever
// Load decodes strictly.
func JSONSchema() ([]byte, error) {
	g := &schemaGen{defs: map[string]any{}}

	root := g.object(reflect.TypeFor[Config]())
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaURL
	root["title"] = "talman.yaml"
	root["description"] = "A talman cluster config, schema " + APIVersion + "."
	root["$defs"] = g.defs

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}

	return append(out, '\n'), nil
}

type schemaGen struct {
	defs map[string]any
}

// schemaRequired lists the keys Validate insists on, by type.
var schemaRequired = map[reflect.Type][]string{
	reflect.TypeFor[Config]():            {"apiVersion", "clusterName", "endpoint", "talosVersion", "kubernetesVersion", "nodes"},
	reflect.TypeFor[Rollout]():           {"waves"},
	reflect.TypeFor[Node]():              {"hostname", "ipAddress", "role"},
	reflect.TypeFor[factory.MetaValue](): {"key", "value"},
}

// schemaDefNames names the types that get a $defs entry of their own.
var schemaDefNames = map[reflect.Type]string{
	reflect.TypeFor[Node]():                     "node",
	reflect.TypeFor[Rollout]():                  "rollout",
	reflect.TypeFor[factory.Config]():           "imageFactory",
	reflect.TypeFor[factory.Schematic]():        "schematic",
	reflect.TypeFor[factory.Customization]():    "customization",
	reflect.TypeFor[factory.Overlay]():          "overlay",
	reflect.TypeFor[factory.MetaValue]():        "metaValue",
	reflect.TypeFor[factory.SystemExtensions](): "systemExtensions",
	reflect.TypeFor[factory.SecureBoot]():       "schematicSecureBoot",
	reflect.TypeFor[factory.DiskImage]():        "diskImage",
}

// schemaDescriptions are the hover text an editor shows, by type and key.
var schemaDescriptions = map[string]string{
	"Config.apiVersion":        "The schema this file is written against.",
	"Config.clusterName":       "The cluster's name, as talosctl gen config takes it.",
	"Config.endpoint":          "The Kubernetes API URL: https, with a port.",
	"Config.talosVersion":      "The Talos version contract configs are rendered for. Pinning it makes renders reproducible.",
	"Config.kubernetesVersion": "The Kubernetes version configs are rendered for, and upgrade-k8s upgrades to.",
	"Config.talosctl":          "The talosctl binary talman drives. Default: talosctl on PATH.",
	"Config.outputDir":         "Where rendered configs, the talosconfig and the kubeconfig are written. Default: clusterconfig.",
	"Config.secretFile":        "The (usually SOPS-encrypted) Talos secrets bundle. Default: secrets.sops.yaml.",
	"Config.validationMode":    "The talosctl validate --mode. Default: metal for the metal platform, cloud otherwise.",
	"Config.values":            "Free-form data, available to every patch template as .Values.",
	"Config.valuesFiles":       "YAML files merged into values, in order and beneath the inline map. Plaintext only.",
	"Node.valuesFiles":         "YAML files merged into this node's values, in order and beneath its inline map.",
	"Config.imageFactory":      "Where installer images come from and how their references are spelled.",
	"Config.schematic":         "The Image Factory schematic: inline, or a path to a (templated) file.",
	"Config.schematicID":       "A schematic ID to use as is, instead of a schematic.",
	"Config.patches":           "Patch files by group: all, controlplane, worker, or a group some node declares. Applied in that order.",
	"Config.nodes":             "Every machine in the cluster.",
	"Config.rollout":           "The order upgrade, reboot and apply take through the cluster, in waves of groups.",
	"Rollout.soak":             "How long to wait after each wave before the next, e.g. 10m.",
	"Rollout.waves":            "Waves in order. A node goes in the first that names its role or a group of its; the rest go last.",
	"Node.hostname":            "An RFC 1123 host name: the Kubernetes node name, and the rendered file's name.",
	"Node.ipAddress":           "The address talman reaches the node at.",
	"Node.role":                "controlplane or worker.",
	"Node.groups":              "Groups whose patches this node receives, applied in this order after its role's.",
	"Node.values":              "Free-form data, available to this node's patch templates as .Node.Values.",
	"Node.patches":             "This node's own patches, applied last.",
	"Node.talosVersion":        "Overrides the cluster's talosVersion for this node.",
	"Node.schematic":           "Overrides the cluster's schematic for this node.",
	"Node.schematicID":         "Overrides the cluster's schematic with an ID for this node.",
	"Node.imageFactory":        "Overrides the cluster's imageFactory key by key for this node.",
	"Config.registryURL":       "The Image Factory host. Default: factory.talos.dev.",
	"Config.protocol":          "The protocol schematics are submitted over. Default: https.",
	"Config.schematicEndpoint": "The path schematics are submitted to. Default: /schematics.",
	"Config.installerURLTmpl":  "The installer image reference, as a Go template over .RegistryURL, .Protocol, .ID, .Version, .Platform (or .Mode) and .SecureBoot.",
	"Config.platform":          "The installer's platform: metal, openstack, aws, …. Default: metal.",
	"Config.secureBoot":        "Use the secure boot installer. Default: false.",
}

// schema returns the schema for t, as a $ref when t has a definition.
func (g *schemaGen) schema(t reflect.Type) any {
	switch t {
	case reflect.TypeFor[Role]():
		return map[string]any{"enum": []string{string(RoleControlPlane), string(RoleWorker)}}
	case reflect.TypeFor[factory.Bootloader]():
		// Any case, as Load reads it: a pattern, since an enum matches
		// exactly and JSON Schema's regular expressions have no flag for it.
		names := []string{"none", "dual-boot", "sd-boot", "grub"}

		alts := make([]string, 0, len(names))
		for _, n := range names {
			alts = append(alts, anyCase(n))
		}

		return map[string]any{
			"type":        "string",
			"pattern":     "^(" + strings.Join(alts, "|") + ")$",
			"description": "none, dual-boot, sd-boot or grub, in any case.",
		}
	case reflect.TypeFor[Wave]():
		groups := map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1}

		return map[string]any{"oneOf": []any{
			map[string]any{"type": "string", "description": "One group, or a role."},
			withDescription(groups, "Several groups, rolled out together."),
		}}
	case reflect.TypeFor[SchematicRef]():
		return map[string]any{"oneOf": []any{
			map[string]any{"type": "string", "minLength": 1, "description": "A path to a schematic file."},
			g.schema(reflect.TypeFor[factory.Schematic]()),
		}}
	}

	switch t.Kind() {
	case reflect.Pointer:
		return g.schema(t.Elem())
	case reflect.String:
		// A number too: `kubernetesVersion: 1.37` is read as the text it
		// was written as, and an editor must not flag what talman loads.
		return map[string]any{"type": []string{"string", "number"}}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Uint8:
		return map[string]any{"type": "integer", "minimum": 0, "maximum": 255}
	case reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		if t.Elem().Kind() == reflect.Interface {
			return map[string]any{"type": "object"}
		}

		// A value left empty -- `patches: {all: }` -- loads as nothing.
		return map[string]any{"type": "object", "additionalProperties": nullable(g.schema(t.Elem()))}
	case reflect.Interface:
		return map[string]any{}
	case reflect.Struct:
		name, ok := schemaDefNames[t]
		if !ok {
			return g.object(t)
		}

		if _, done := g.defs[name]; !done {
			g.defs[name] = nil // placeholder, for types that refer to themselves
			g.defs[name] = g.object(t)
		}

		return map[string]any{"$ref": "#/$defs/" + name}
	default:
		panic(fmt.Sprintf("jsonschema: no mapping for %s", t))
	}
}

// object describes a struct by its yaml tags. Every one talman decodes
// strictly, so no other key is allowed.
func (g *schemaGen) object(t reflect.Type) map[string]any {
	props := map[string]any{}

	for f := range t.Fields() {
		tag := f.Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")

		if name == "-" || !f.IsExported() {
			continue
		}

		if name == "" {
			name = strings.ToLower(f.Name)
		}

		s := g.schema(f.Type)

		// Validate's own rules, where a pattern or an enum can say them.
		if rule, ok := schemaRules[t.Name()+"."+name]; ok {
			m, _ := s.(map[string]any)

			out := map[string]any{"type": "string"}

			for k, v := range m {
				if k != "type" {
					out[k] = v
				}
			}

			for k, v := range rule {
				out[k] = v
			}

			s = out
		}

		if n, ok := schemaMinItems[t.Name()+"."+name]; ok {
			if m, isMap := s.(map[string]any); isMap {
				m["minItems"] = n
			}
		}

		if d, ok := schemaDescriptions[t.Name()+"."+name]; ok {
			s = withDescription(s, d)
		}

		// An optional key left empty is YAML's null, which Load reads as the
		// key's zero value: accepted, so allowed here too.
		if !slices.Contains(schemaRequired[t], name) {
			s = nullable(s)
		}

		props[name] = s
	}

	obj := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}

	if t == reflect.TypeFor[Config]() {
		props["apiVersion"] = withDescription(map[string]any{"const": APIVersion},
			schemaDescriptions["Config.apiVersion"])
	}

	if req := schemaRequired[t]; len(req) > 0 {
		obj["required"] = req
	}

	// schematic and schematicID are two ways of saying one thing, and Validate
	// refuses both at once, at the cluster level and on a node.
	if t == reflect.TypeFor[Config]() || t == reflect.TypeFor[Node]() {
		obj["not"] = map[string]any{"required": []string{"schematic", "schematicID"}}
	}

	return obj
}

// schemaRules are Validate's checks that a schema can express, by type and
// key, so that an editor refuses what talman would.
var schemaRules = map[string]map[string]any{
	"Config.endpoint":       {"pattern": `^https://[^/]+:[0-9]+(/.*)?$`},
	"Config.clusterName":    {"pattern": clusterNamePattern.String(), "maxLength": 253},
	"Config.validationMode": {"enum": validValidationModes},
	"Node.hostname": {
		"pattern":   `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`,
		"maxLength": 253,
	},
	"Rollout.soak": {"pattern": `^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`},
}

// schemaMinItems are the lists Validate refuses empty.
var schemaMinItems = map[string]int{
	"Config.nodes":  1,
	"Rollout.waves": 1,
}

// nullable lets s also be null.
func nullable(s any) any {
	m, ok := s.(map[string]any)
	if !ok {
		return s
	}

	switch t := m["type"].(type) {
	case string:
		out := maps.Clone(m)
		out["type"] = []string{t, "null"}

		return out
	case []string:
		out := maps.Clone(m)
		out["type"] = append(slices.Clone(t), "null")

		return out
	}

	return map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
}

// withDescription attaches a description. A $ref cannot carry siblings in
// every validator, so it is wrapped.
func withDescription(s any, d string) any {
	m, ok := s.(map[string]any)
	if !ok {
		return s
	}

	if _, isRef := m["$ref"]; isRef {
		return map[string]any{"allOf": []any{m}, "description": d}
	}

	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}

	out["description"] = d

	return out
}

// anyCase spells s as a regular expression matching it in any case: "sd"
// becomes "[sS][dD]".
func anyCase(s string) string {
	var b strings.Builder

	for _, r := range s {
		lo, up := strings.ToLower(string(r)), strings.ToUpper(string(r))
		if lo == up {
			b.WriteString(regexp.QuoteMeta(string(r)))

			continue
		}

		b.WriteString("[" + lo + up + "]")
	}

	return b.String()
}
