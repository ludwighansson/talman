package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
)

// validTalosModes are the modes `talosctl validate --mode` accepts.
var validTalosModes = []string{"metal", "cloud", "container"}

// Validate checks the whole config and reports every problem at once.
//
// Failing on the first error would mean an operator fixes one path, re-runs,
// and finds the next -- for a config that is mostly a list of file paths, that
// is a poor trade against one pass listing everything.
func (c *Config) Validate() error {
	var errs []error

	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	c.normalize()

	if c.ClusterName == "" {
		add("clusterName is required")
	}

	c.validateEndpoint(add)

	if c.TalosVersion == "" {
		add("talosVersion is required: pinning it is what makes renders reproducible " +
			"(an unset version silently adopts whatever contract the local talosctl defaults to)")
	}

	// A config naming a schema talman does not know is refused rather than
	// read hopefully: the fields it does recognise may mean something else
	// there, and guessing at a machine configuration is how a cluster gets a
	// setting nobody wrote.
	if c.APIVersion != "" && c.APIVersion != APIVersion {
		add("apiVersion %q is not one this talman understands: it speaks %q", c.APIVersion, APIVersion)
	}

	if c.KubernetesVersion == "" {
		add("kubernetesVersion is required")
	}

	if !slices.Contains(validTalosModes, c.TalosMode) {
		add("talosMode %q is invalid: must be one of %s", c.TalosMode, strings.Join(validTalosModes, ", "))
	}

	if c.Schematic != nil && c.SchematicID != "" {
		add("schematic and schematicID are mutually exclusive at the cluster level")
	}

	c.validateNodes(add)
	c.validatePatchKeys(add)
	c.validatePatchFiles(add)

	return errors.Join(errs...)
}

// normalize fixes up spellings that are unambiguous, so validation only
// reports things the operator genuinely has to decide about.
func (c *Config) normalize() {
	// Talos version contracts are always v-prefixed; kubernetesVersion is
	// accepted either way by talosctl.
	if c.TalosVersion != "" && !strings.HasPrefix(c.TalosVersion, "v") {
		c.TalosVersion = "v" + c.TalosVersion
	}

	for i := range c.Nodes {
		if v := c.Nodes[i].TalosVersion; v != "" && !strings.HasPrefix(v, "v") {
			c.Nodes[i].TalosVersion = "v" + v
		}
	}
}

func (c *Config) validateEndpoint(add func(string, ...any)) {
	if c.Endpoint == "" {
		add("endpoint is required (the Kubernetes API URL, e.g. https://10.0.0.1:6443)")

		return
	}

	// url.Parse's own message for a missing scheme is "first path segment in
	// URL cannot contain colon", which tells the operator nothing. Both the
	// parse failure and a scheme-less parse mean the same fix.
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		add("endpoint %q must be a full URL including scheme and port, e.g. https://10.0.0.1:6443", c.Endpoint)
	}
}

func (c *Config) validateNodes(add func(string, ...any)) {
	if len(c.Nodes) == 0 {
		add("nodes is required and must list at least one node")

		return
	}

	if len(c.ControlPlanes()) == 0 {
		add("at least one node must have role: %s", RoleControlPlane)
	}

	seenHost := map[string]int{}
	seenIP := map[string]string{}

	for i := range c.Nodes {
		n := &c.Nodes[i]
		where := fmt.Sprintf("nodes[%d]", i)

		switch n.Hostname {
		case "":
			add("%s: hostname is required", where)
		default:
			if prev, dup := seenHost[n.Hostname]; dup {
				add("%s: duplicate hostname %q (also nodes[%d])", where, n.Hostname, prev)
			}

			seenHost[n.Hostname] = i
			where = "node " + n.Hostname
		}

		if n.IPAddress == "" {
			add("%s: ipAddress is required (talman passes it verbatim to talosctl --nodes)", where)
		} else if prev, dup := seenIP[n.IPAddress]; dup {
			add("%s: duplicate ipAddress %q (also on %s)", where, n.IPAddress, prev)
		} else {
			seenIP[n.IPAddress] = where
		}

		if n.Role == "" {
			add("%s: role is required: %s or %s", where, RoleControlPlane, RoleWorker)
		}

		for _, g := range n.Groups {
			if isReserved(g) {
				add("%s: group %q is reserved: %s, %s and %s are assigned automatically from role",
					where, g, GroupAll, GroupControlPlane, GroupWorker)
			}
		}

		if n.Schematic != nil && n.SchematicID != "" {
			add("%s: schematic and schematicID are mutually exclusive", where)
		}
	}
}

// validatePatchKeys enforces the rule that every key in the patches block is
// either reserved or a group some node actually declares. Without it, renaming
// a group on the nodes leaves an orphaned patches key that silently applies to
// nothing.
func (c *Config) validatePatchKeys(add func(string, ...any)) {
	declared := c.DeclaredGroups()

	var unknown []string

	for key := range c.Patches {
		if isReserved(key) || declared[key] {
			continue
		}

		unknown = append(unknown, key)
	}

	if len(unknown) == 0 {
		return
	}

	sort.Strings(unknown)

	known := []string{GroupAll, GroupControlPlane, GroupWorker}
	for g := range declared {
		known = append(known, g)
	}

	sort.Strings(known)

	for _, key := range unknown {
		add("patches.%s: no node declares the group %q; known keys are %s",
			key, key, strings.Join(known, ", "))
	}
}

// validatePatchFiles is the check the whole design hangs on: a patch key's
// value must resolve to a real file. talosctl's own message for a missing
// patch is "error parsing config JSON patch: open x: no such file or
// directory", which points at the wrong thing entirely.
func (c *Config) validatePatchFiles(add func(string, ...any)) {
	for _, ref := range c.AllPatchPaths() {
		info, err := os.Stat(ref.Path)

		switch {
		case os.IsNotExist(err):
			add("patches.%s: %q does not exist (resolved to %s)", ref.Group, ref.Rel, ref.Path)
		case err != nil:
			add("patches.%s: %q is not readable: %v", ref.Group, ref.Rel, err)
		case info.IsDir():
			add("patches.%s: %q is a directory; patches must be individual files "+
				"so the apply order is explicit", ref.Group, ref.Rel)
		case !info.Mode().IsRegular():
			add("patches.%s: %q is not a regular file", ref.Group, ref.Rel)
		}
	}
}

func isReserved(key string) bool {
	switch key {
	case GroupAll, GroupControlPlane, GroupWorker:
		return true
	default:
		return false
	}
}
