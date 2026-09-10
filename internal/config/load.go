package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

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

	return cfg, nil
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

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config

	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
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

	if c.TalosMode == "" {
		c.TalosMode = "metal"
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

// FindConfig locates a config file: the explicit path if given, else
// talman.yaml in the working directory.
func FindConfig(explicit string) string {
	if explicit != "" {
		return explicit
	}

	return DefaultFileName
}
