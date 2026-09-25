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

		dec := yaml.NewDecoder(bytes.NewReader(data))

		if err := dec.Decode(&values); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("valuesFiles: %s: %w", rel, err)
		}

		if more, err := moreDocuments(dec); err != nil {
			return nil, fmt.Errorf("valuesFiles: %s: %w", rel, err)
		} else if more {
			return nil, fmt.Errorf("valuesFiles: %s holds more than one YAML document; "+
				"only the first would be read", rel)
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
		dst[k] = merge(dst[k], v)
	}
}

// merge returns src merged over dst. Both kinds of map YAML decodes into take
// part -- map[string]any, and map[any]any for a mapping with a key that is not
// a string -- and keep the keys they were decoded with, so a template indexes
// them the way the YAML spelled them.
func merge(dst, src any) any {
	switch s := src.(type) {
	case map[string]any:
		switch d := dst.(type) {
		case map[string]any:
			for k, v := range s {
				d[k] = merge(d[k], v)
			}

			return d
		case map[any]any:
			for k, v := range s {
				d[k] = merge(d[k], v)
			}

			return d
		}

		out := make(map[string]any, len(s))
		for k, v := range s {
			out[k] = merge(nil, v)
		}

		return out
	case map[any]any:
		out := map[any]any{}

		switch d := dst.(type) {
		case map[any]any:
			out = d
		case map[string]any:
			for k, v := range d {
				out[k] = v
			}
		}

		for k, v := range s {
			out[k] = merge(out[k], v)
		}

		return out
	default:
		return src
	}
}

// CopyValues is a deep copy of a values map: maps and lists all the way down,
// so a template that changes its copy -- sprig's set, unset, merge and
// mergeOverwrite all work in place -- changes nothing another node, or the
// config, will see.
func CopyValues(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}

	out, _ := copyValue(v).(map[string]any)

	return out
}

func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = copyValue(x)
		}

		return out
	case map[any]any:
		out := make(map[any]any, len(t))
		for k, x := range t {
			out[k] = copyValue(x)
		}

		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = copyValue(x)
		}

		return out
	default:
		return v
	}
}
