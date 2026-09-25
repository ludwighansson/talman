package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Load reads, strictly decodes and validates a talman config file.
//
// Strict decoding matters more than it looks: a mistyped key in a config whose
// whole job is to route patch files would otherwise be silently ignored and
// produce a machine config missing a patch.
func Load(path string) (*Config, error) {
	cfg, err := LoadNoValidate(path)
	if err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	if err := cfg.mergeValuesFiles(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Rel shortens a path against the working directory for a message.
func Rel(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}

	rel, err := filepath.Rel(wd, path)
	if err != nil || len(rel) > len(path) {
		return path
	}

	return rel
}

// LoadNoValidate reads and decodes without running semantic validation. Used
// by commands that want to report every problem themselves.
func LoadNoValidate(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no talman config at %s (use -c to point at one)", path)
		}

		return nil, err
	}

	// The schema version is read on its own, and loosely, before anything is
	// decoded strictly. A file written for a later schema is all but certain
	// to carry a key this one does not have, and a strict decode would report
	// that key -- "field x not found" -- rather than the reason, which is that
	// the file is not one this talman can read at all.
	var head struct {
		APIVersion string `yaml:"apiVersion"`
	}

	if err := yaml.Unmarshal(data, &head); err == nil && head.APIVersion != "" && head.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q is not one this talman understands: it speaks %q "+
			"(a newer schema needs a newer talman)", path, head.APIVersion, APIVersion)
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config

	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s is empty", path)
		}

		return nil, fmt.Errorf("parsing %s: %w%s", path, err, renameHint(data))
	}

	// A second document would be ignored, and whatever it says with it: two
	// clusters pasted into one file, or a patch saved over the config.
	// An empty one -- a trailing `---`, or a document of comments -- is
	// not a second config, and plenty of generated YAML ends that way.
	if more, err := moreDocuments(dec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	} else if more {
		return nil, fmt.Errorf("parsing %s: it holds more than one YAML document; "+
			"a talman config is exactly one", path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	cfg.Path = abs
	cfg.Dir = filepath.Dir(abs)
	cfg.applyDefaults()

	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Talosctl == "" {
		c.Talosctl = DefaultTalosctl
	}

	if c.OutputDir == "" {
		c.OutputDir = DefaultOutputDir
	}

	if c.SecretFile == "" {
		c.SecretFile = DefaultSecretFile
	}

	c.ImageFactory = c.ImageFactory.WithDefaults()
}

// OutputPath is the absolute output directory.
func (c *Config) OutputPath() string { return c.resolvePath(c.OutputDir) }

// SecretPath is the absolute path of the secrets bundle.
func (c *Config) SecretPath() string { return c.resolvePath(c.SecretFile) }

// MachineConfigPath is where a node's rendered machine config is written.
//
// Named by hostname alone: the output directory already belongs to one
// cluster, and hostnames in practice carry the cluster name themselves, so a
// cluster prefix just yields development-development-worker-01.yaml.
func (c *Config) MachineConfigPath(n *Node) string {
	return filepath.Join(c.OutputPath(), n.Hostname+".yaml")
}

// TalosconfigPath is where the generated talosconfig is written.
func (c *Config) TalosconfigPath() string {
	return filepath.Join(c.OutputPath(), "talosconfig")
}

// KubeconfigPath is where the fetched kubeconfig is written.
//
// Beside the talosconfig, and for the same reason: it is a per-cluster
// credential that belongs to this cluster directory, not to whatever
// ~/.kube/config happens to hold.
func (c *Config) KubeconfigPath() string {
	return filepath.Join(c.OutputPath(), "kubeconfig")
}

// EnvConfig names the config file when -c does not, so a CI job can set it
// once for every step.
const EnvConfig = "TALMAN_CONFIG"

// FindConfig locates a config file: the explicit path if given, else
// $TALMAN_CONFIG, else talman.yaml in the working directory.
func FindConfig(explicit string) string {
	if explicit != "" {
		return explicit
	}

	if env := os.Getenv(EnvConfig); env != "" {
		return env
	}

	return DefaultFileName
}

// renameHint names the keys 1.0 renamed that a config still spells the old
// way, for the error a strict decode gives.
//
// Read from the file, loosely, rather than out of the decoder's error: that
// wording is the YAML library's, and a new release of it rewording the error
// would drop the hint without a word.
func renameHint(data []byte) string {
	var raw struct {
		TalosMode    any            `yaml:"talosMode"`
		ImageFactory map[string]any `yaml:"imageFactory"`
		Nodes        []struct {
			ImageFactory map[string]any `yaml:"imageFactory"`
		} `yaml:"nodes"`
	}

	if err := yaml.Unmarshal(data, &raw); err != nil {
		return ""
	}

	var hints []string

	if raw.TalosMode != nil {
		hints = append(hints, "talosMode was renamed validationMode in 1.0, and can usually be dropped: "+
			"it follows imageFactory.platform")
	}

	secureboot := func(m map[string]any) bool { _, ok := m["secureboot"]; return ok }

	old := secureboot(raw.ImageFactory)
	for _, n := range raw.Nodes {
		old = old || secureboot(n.ImageFactory)
	}

	if old {
		hints = append(hints, "imageFactory.secureboot was renamed secureBoot in 1.0")
	}

	if len(hints) == 0 {
		return ""
	}

	return "\n  " + strings.Join(hints, "\n  ")
}

// moreDocuments reports whether the decoder holds another document with
// anything in it.
func moreDocuments(dec *yaml.Decoder) (bool, error) {
	for {
		var doc yaml.Node

		err := dec.Decode(&doc)

		switch {
		case errors.Is(err, io.EOF):
			return false, nil
		case err != nil:
			return false, err
		case !emptyDocument(&doc):
			return true, nil
		}
	}
}

func emptyDocument(doc *yaml.Node) bool {
	if doc.Kind == yaml.DocumentNode {
		for _, c := range doc.Content {
			if !emptyDocument(c) {
				return false
			}
		}

		return true
	}

	return doc.Kind == 0 || (doc.Kind == yaml.ScalarNode && doc.Tag == "!!null" && doc.Value == "")
}
