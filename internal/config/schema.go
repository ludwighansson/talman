// Package config defines and loads talman's cluster configuration.
//
// The schema is deliberately small. talman never mirrors a Talos machine
// configuration struct: everything Talos-shaped is expressed as a patch file,
// which is why a new Talos release cannot break this package.
package config

import (
	"fmt"

	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/factory"
)

// Reserved patch group keys. Every other key in the patches block must name a
// group that at least one node declares.
const (
	GroupAll          = "all"
	GroupControlPlane = "controlplane"
	GroupWorker       = "worker"
)

// Defaults applied when the corresponding field is unset.
const (
	DefaultFileName   = "talman.yaml"
	DefaultOutputDir  = "clusterconfig"
	DefaultSecretFile = "secrets.sops.yaml"
	DefaultTalosctl   = "talosctl"
)

// Role is a node's Talos machine type.
type Role string

// The only two roles Talos has.
const (
	RoleControlPlane Role = "controlplane"
	RoleWorker       Role = "worker"
)

// UnmarshalYAML rejects any role other than the two Talos actually has, so a
// typo surfaces at load time instead of as a confusing talosctl error.
func (r *Role) UnmarshalYAML(value *yaml.Node) error {
	var s string

	if err := value.Decode(&s); err != nil {
		return err
	}

	switch Role(s) {
	case RoleControlPlane, RoleWorker:
		*r = Role(s)

		return nil
	default:
		return fmt.Errorf("line %d: invalid role %q: must be %q or %q",
			value.Line, s, RoleControlPlane, RoleWorker)
	}
}

// APIVersion is the schema this talman speaks.
//
// It exists so a later, incompatible schema can be told apart from this one
// rather than misread: without it, the choice when the shape has to change is
// between breaking every config silently and never changing it. It is
// required, so that no config is left for a later talman to guess about.
const APIVersion = "talman.dev/v1"

// Config is the whole of talman.yaml.
type Config struct {
	// APIVersion names the schema. Required.
	APIVersion string `yaml:"apiVersion"`

	ClusterName       string `yaml:"clusterName"`
	Endpoint          string `yaml:"endpoint"`
	TalosVersion      string `yaml:"talosVersion"`
	KubernetesVersion string `yaml:"kubernetesVersion"`

	// Talosctl is the binary talman shells out to. Everything that touches the
	// Talos API goes through it.
	Talosctl string `yaml:"talosctl,omitempty"`
	// OutputDir is where rendered machine configs and the talosconfig land.
	OutputDir string `yaml:"outputDir,omitempty"`
	// SecretFile is the (usually SOPS-encrypted) Talos secrets bundle.
	SecretFile string `yaml:"secretFile,omitempty"`
	// ValidationMode is the --mode `talosctl validate` checks rendered
	// configs against. Unset, it follows the node's image platform: metal for
	// metal, cloud for everything else. Only a container cluster -- the
	// docker provisioner's -- has to say so.
	ValidationMode string `yaml:"validationMode,omitempty"`

	// Values is cluster-wide free-form data, exposed to every patch template
	// as .Values. Node.Values is the per-node counterpart.
	Values map[string]any `yaml:"values,omitempty"`
	// ValuesFiles are YAML files merged into Values, in order and beneath the
	// inline map, so values several clusters share can live in one place.
	ValuesFiles []string `yaml:"valuesFiles,omitempty"`

	ImageFactory factory.Config `yaml:"imageFactory,omitempty"`
	Schematic    *SchematicRef  `yaml:"schematic,omitempty"`
	SchematicID  string         `yaml:"schematicID,omitempty"`

	// Patches maps a group key to an ordered list of patch file paths. Keys are
	// `all`, `controlplane`, `worker`, or any group named by a node.
	Patches map[string][]string `yaml:"patches,omitempty"`

	Nodes []Node `yaml:"nodes"`

	// Rollout orders the nodes a roll-out reaches -- upgrade, reboot, a
	// real apply -- in waves of groups. Without it, config order.
	Rollout *Rollout `yaml:"rollout,omitempty"`

	// Dir is the directory holding the config file; every relative patch path
	// resolves against it. Not settable from YAML.
	Dir string `yaml:"-"`
	// Path is the config file talman loaded. Not settable from YAML.
	Path string `yaml:"-"`
}

// Node is one machine. It carries only what talman itself needs to route
// patches and reach the host; everything else belongs in a patch.
type Node struct {
	Hostname  string   `yaml:"hostname"`
	IPAddress string   `yaml:"ipAddress"`
	Role      Role     `yaml:"role"`
	Groups    []string `yaml:"groups,omitempty"`
	// Values is per-node free-form data, exposed to that node's patch
	// templates as .Node.Values.
	Values map[string]any `yaml:"values,omitempty"`
	// ValuesFiles are merged into Values the way the cluster's are.
	ValuesFiles []string `yaml:"valuesFiles,omitempty"`
	Patches     []string `yaml:"patches,omitempty"`

	// TalosVersion overrides the cluster version for this node, for staged
	// upgrades across a mixed-version cluster.
	TalosVersion string        `yaml:"talosVersion,omitempty"`
	Schematic    *SchematicRef `yaml:"schematic,omitempty"`
	SchematicID  string        `yaml:"schematicID,omitempty"`
	// ImageFactory overrides the cluster's imageFactory field by field, for a
	// cluster whose machines do not all boot the same platform's image.
	ImageFactory *factory.Config `yaml:"imageFactory,omitempty"`
}

// SchematicRef is either an inline schematic or a path to a schematic file.
// A file is rendered as a template like any patch, so a schematic can vary per
// node.
type SchematicRef struct {
	// Path is set when the YAML value was a scalar.
	Path string
	// Inline is set when the YAML value was a mapping.
	Inline *factory.Schematic
}

// UnmarshalYAML accepts either form.
func (s *SchematicRef) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var p string

		if err := value.Decode(&p); err != nil {
			return err
		}

		if p == "" {
			return fmt.Errorf("line %d: schematic path is empty", value.Line)
		}

		s.Path = p

		return nil
	case yaml.MappingNode:
		var inline factory.Schematic

		// KnownFields is not available through a yaml.Node decode, so round-trip
		// through bytes to get the strict behaviour factory.Unmarshal provides.
		raw, err := yaml.Marshal(value)
		if err != nil {
			return err
		}

		parsed, err := factory.Unmarshal(raw)
		if err != nil {
			return fmt.Errorf("line %d: invalid schematic: %w", value.Line, err)
		}

		inline = *parsed
		s.Inline = &inline

		return nil
	default:
		return fmt.Errorf("line %d: schematic must be a file path or an inline mapping", value.Line)
	}
}

// IsControlPlane reports whether the node runs the control plane.
func (n *Node) IsControlPlane() bool { return n.Role == RoleControlPlane }

// EffectiveTalosVersion is the node override if set, else the cluster version.
func (n *Node) EffectiveTalosVersion(c *Config) string {
	if n.TalosVersion != "" {
		return n.TalosVersion
	}

	return c.TalosVersion
}

// ImageFactoryFor is the cluster's imageFactory with the node's overrides
// applied.
func (c *Config) ImageFactoryFor(n *Node) factory.Config {
	return c.ImageFactory.Override(n.ImageFactory).WithDefaults()
}

// ValidationModeFor is the `talosctl validate --mode` a node's config is
// checked against.
func (c *Config) ValidationModeFor(n *Node) string {
	if c.ValidationMode != "" {
		return c.ValidationMode
	}

	if c.ImageFactoryFor(n).Platform == "metal" {
		return "metal"
	}

	return "cloud"
}

// ControlPlanes returns the control plane nodes in declaration order.
func (c *Config) ControlPlanes() []*Node {
	var out []*Node

	for i := range c.Nodes {
		if c.Nodes[i].IsControlPlane() {
			out = append(out, &c.Nodes[i])
		}
	}

	return out
}

// Node looks a node up by hostname, then by IP address.
func (c *Config) Node(name string) (*Node, bool) {
	for i := range c.Nodes {
		if c.Nodes[i].Hostname == name {
			return &c.Nodes[i], true
		}
	}

	for i := range c.Nodes {
		if c.Nodes[i].IPAddress == name {
			return &c.Nodes[i], true
		}
	}

	return nil, false
}

// DeclaredGroups is the set of every group named by any node.
func (c *Config) DeclaredGroups() map[string]bool {
	groups := map[string]bool{}

	for i := range c.Nodes {
		for _, g := range c.Nodes[i].Groups {
			groups[g] = true
		}
	}

	return groups
}
