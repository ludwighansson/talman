// Package patch inspects rendered patch documents before they reach talosctl.
package patch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v4"
)

// CheckStrategicMerge rejects RFC6902 JSON patches.
//
// Talos generates multi-document machine configs from v1.12 onwards, and
// configpatcher refuses JSON6902 against multi-doc input. Left alone, the
// operator sees "JSON6902 patches are not supported for multi-document machine
// configuration" with no indication of which of their files caused it.
func CheckStrategicMerge(name string, data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))

	for i := 0; ; i++ {
		var doc yaml.Node

		err := dec.Decode(&doc)
		if err != nil {
			// errors.Is, not a string compare: the YAML module is pinned to a
			// release candidate that renovate is configured to bump, and a
			// reworded terminal error would otherwise turn every patch in the
			// tree into "not valid YAML".
			if errors.Is(err, io.EOF) {
				return nil
			}

			return fmt.Errorf("%s: document %d is not valid YAML: %w", name, i, err)
		}

		content := &doc
		if content.Kind == yaml.DocumentNode && len(content.Content) == 1 {
			content = content.Content[0]
		}

		if looksLikeJSON6902(content) {
			return fmt.Errorf("%s: document %d is an RFC6902 JSON patch (a list of op/path entries), "+
				"which Talos cannot apply to a multi-document machine config; "+
				"rewrite it as a strategic merge patch, using `$patch: delete` to remove a field or document",
				name, i)
		}
	}
}

func looksLikeJSON6902(n *yaml.Node) bool {
	if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		return false
	}

	first := n.Content[0]
	if first.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i+1 < len(first.Content); i += 2 {
		if strings.EqualFold(first.Content[i].Value, "op") {
			return true
		}
	}

	return false
}
