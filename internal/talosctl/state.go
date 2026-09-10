package talosctl

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"
)

// NodeState is what talman can learn about a running node through talosctl.
//
// Fields are best-effort: a node in maintenance mode, or one running a Talos
// old enough to lack a resource, leaves them empty rather than failing. The
// callers treat "unknown" as "cannot prove it is up to date".
type NodeState struct {
	// TalosVersion is the tag the node is running, v-prefixed.
	TalosVersion string
	// SchematicID is the Image Factory schematic the running image was built
	// from, as reported by the schematic extension.
	SchematicID string
}

// State reads a node's running Talos version and schematic.
func (r *Runner) State(talosconfig, node string) (NodeState, error) {
	var state NodeState

	raw, err := r.Output("--talosconfig", talosconfig, "--nodes", node, "version")
	if err != nil {
		return state, err
	}

	state.TalosVersion = parseServerTag(string(raw))

	// A node with no schematic extension (not installed from a factory image)
	// is not an error; it just cannot be compared on schematic.
	ext, err := r.Output("--talosconfig", talosconfig, "--nodes", node,
		"get", "extensions", "--output", "yaml")
	if err == nil {
		state.SchematicID = parseSchematicID(ext)
	}

	return state, nil
}

// parseServerTag pulls the server-side tag out of `talosctl version`.
//
// The output carries a Client block and a Server block, each with a Tag, so
// taking the first match would report the local talosctl version as if it
// were the node's:
//
//	Client:
//	        Tag:         v1.14.0
//	Server:
//	        NODE:        10.0.0.11
//	        Tag:         v1.13.5
func parseServerTag(out string) string {
	inServer := false

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Server:"):
			inServer = true
		case strings.HasPrefix(trimmed, "Client:"):
			inServer = false
		case inServer && strings.HasPrefix(trimmed, "Tag:"):
			tag := strings.TrimSpace(strings.TrimPrefix(trimmed, "Tag:"))
			if tag != "" && !strings.HasPrefix(tag, "v") {
				tag = "v" + tag
			}

			return tag
		}
	}

	return ""
}

// parseSchematicID finds the schematic extension in `talosctl get extensions`.
//
// The walk is deliberately structural rather than tied to a field path: the
// resource layout is Talos', not talman's, and a version bump that nests it
// differently should degrade to "unknown" rather than to a wrong answer.
func parseSchematicID(out []byte) string {
	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc any

		if err := dec.Decode(&doc); err != nil {
			return ""
		}

		if id := findSchematic(doc); id != "" {
			return id
		}
	}
}

func findSchematic(node any) string {
	switch v := node.(type) {
	case map[string]any:
		if name, _ := v["name"].(string); name == "schematic" {
			if version, ok := v["version"].(string); ok && version != "" {
				return version
			}
		}

		for _, child := range v {
			if id := findSchematic(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range v {
			if id := findSchematic(child); id != "" {
				return id
			}
		}
	}

	return ""
}

// Reachable reports whether the node answers the Talos API.
func (r *Runner) Reachable(talosconfig, node string) bool {
	_, err := r.Output("--talosconfig", talosconfig, "--nodes", node, "version")

	return err == nil
}

// WaitReady blocks until a node has answered the API continuously for the
// stabilization window, or until timeout.
//
// Continuously, not once: a node that has applied a config reboots, and a
// single successful probe can land in the window before it goes down. Holding
// the check for a settling period is what makes "the node came back" mean it.
func (r *Runner) WaitReady(talosconfig, node string, stabilize, timeout time.Duration, log func(string, ...any)) error {
	const poll = 5 * time.Second

	deadline := time.Now().Add(timeout)

	var steadySince time.Time

	for {
		if r.Reachable(talosconfig, node) {
			if steadySince.IsZero() {
				steadySince = time.Now()

				log("  %s responding; holding %s to confirm it stays up", node, stabilize)
			}

			if time.Since(steadySince) >= stabilize {
				return nil
			}
		} else if !steadySince.IsZero() {
			steadySince = time.Time{}

			log("  %s went away again; restarting the stabilization window", node)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not stay reachable for %s within %s", node, stabilize, timeout)
		}

		time.Sleep(poll)
	}
}
