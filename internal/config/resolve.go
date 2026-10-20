package config

import (
	"path/filepath"
	"sort"
)

// PatchRef is one resolved patch file in a node's chain.
type PatchRef struct {
	// Group is the patches key it came from, or "node" for nodes[].patches.
	Group string
	// Node is the hostname whose own patch it is, for nodes[].patches.
	Node string
	// Rel is the path exactly as written in the config, for error messages.
	Rel string
	// Path is Rel resolved against the config file's directory.
	Path string
}

// SourceNode is the Group value used for per-node patches.
const SourceNode = "node"

// Where is the key in talman.yaml that lists the patch, as a message names it:
// patches.<group>, or nodes[<hostname>].patches for a node's own. A message
// naming a key the config does not have -- patches.node -- sends the reader
// looking for it.
func (r PatchRef) Where() string {
	if r.Node != "" {
		return "nodes[" + r.Node + "].patches"
	}

	return "patches." + r.Group
}

// ResolvePath makes a config-relative path absolute against the config dir.
// An already-absolute path is returned unchanged.
func (c *Config) ResolvePath(rel string) string {
	if filepath.IsAbs(rel) {
		return filepath.Clean(rel)
	}

	return filepath.Join(c.Dir, rel)
}

// PatchChain returns the ordered patch list for a node.
//
// Order is significant: Talos applies strategic merge patches in sequence and
// the last writer wins. The chain is
//
//	all -> <role> -> each of nodes[].groups in declaration order -> nodes[].patches
//
// so a group patch overrides a role patch, and a node's own patch overrides
// everything.
func (c *Config) PatchChain(n *Node) []PatchRef {
	var chain []PatchRef

	add := func(group, node string, paths []string) {
		for _, rel := range paths {
			chain = append(chain, PatchRef{Group: group, Node: node, Rel: rel, Path: c.ResolvePath(rel)})
		}
	}

	add(GroupAll, "", c.Patches[GroupAll])
	add(string(n.Role), "", c.Patches[string(n.Role)])

	for _, g := range n.Groups {
		add(g, "", c.Patches[g])
	}

	add(SourceNode, n.Hostname, n.Patches)

	return chain
}

// AllPatchPaths returns every path mentioned anywhere in the config, each
// tagged with the key that mentioned it. Used by validation to check that
// every declared patch resolves to a file, including ones no node consumes.
func (c *Config) AllPatchPaths() []PatchRef {
	var out []PatchRef

	// Sorted, because Go randomises map iteration and validation sets out to
	// list every problem in one pass -- an order that reshuffles between
	// identical runs cannot be diffed or worked through top to bottom.
	groups := make([]string, 0, len(c.Patches))
	for group := range c.Patches {
		groups = append(groups, group)
	}

	sort.Strings(groups)

	for _, group := range groups {
		for _, rel := range c.Patches[group] {
			out = append(out, PatchRef{Group: group, Rel: rel, Path: c.ResolvePath(rel)})
		}
	}

	for i := range c.Nodes {
		for _, rel := range c.Nodes[i].Patches {
			out = append(out, PatchRef{
				Group: SourceNode,
				Node:  c.Nodes[i].Hostname,
				Rel:   rel,
				Path:  c.ResolvePath(rel),
			})
		}
	}

	return out
}
