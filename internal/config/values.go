package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v4"
)

// mergeValuesFiles folds valuesFiles into values, for the cluster and for
// each node: the files in the order they are listed, then the inline map on
// top, so a cluster can override a shared value by writing it out.
func (c *Config) mergeValuesFiles() error {
	merged, err := c.readValues(c.ValuesFiles, c.Values)
	if err != nil {
		return err
	}

	c.Values = merged

	for i := range c.Nodes {
		n := &c.Nodes[i]

		if merged, err = c.readValues(n.ValuesFiles, n.Values); err != nil {
			return fmt.Errorf("node %s: %w", n.Hostname, err)
		}

		n.Values = merged
	}

	return nil
}

func (c *Config) readValues(files []string, inline map[string]any) (map[string]any, error) {
	if len(files) == 0 {
		return inline, nil
	}

	out := map[string]any{}

	for _, rel := range files {
		data, err := os.ReadFile(c.resolvePath(rel))
		if err != nil {
			return nil, fmt.Errorf("valuesFiles: %w", err)
		}

		var values map[string]any

		if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&values); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("valuesFiles: %s: %w", rel, err)
		}

		// Plaintext only. A secret belongs in an encrypted patch, which
		// validate can check without the key; a secret value would make every
		// template that reads it unverifiable without one.
		if _, sops := values["sops"]; sops {
			return nil, fmt.Errorf("valuesFiles: %s is SOPS-encrypted; values files are read as plain "+
				"YAML, so put secrets in an encrypted patch instead", rel)
		}

		mergeInto(out, values)
	}

	mergeInto(out, inline)

	return out, nil
}

// mergeInto merges src into dst: maps key by key, all the way down, and
// anything else replaced outright.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		sub, isMap := v.(map[string]any)
		have, hadMap := dst[k].(map[string]any)

		if isMap && hadMap {
			mergeInto(have, sub)

			continue
		}

		if isMap {
			fresh := map[string]any{}
			mergeInto(fresh, sub)
			v = fresh
		}

		dst[k] = v
	}
}
