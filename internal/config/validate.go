package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// validValidationModes are the modes `talosctl validate --mode` accepts.
var validValidationModes = []string{"metal", "cloud", "container"}

// clusterNamePattern is what a cluster name may be: something that is safe
// inside a file name.
var clusterNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)

// VersionPattern is a full version, as Talos and Kubernetes images are tagged:
// v1.14.1, or 1.14.1, with an optional pre-release suffix.
var VersionPattern = regexp.MustCompile(versionRE)

const versionRE = `^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`

// hostnameLabelRE is one RFC 1123 label, lower case: what Kubernetes accepts
// as a node name, and safe as the file name the rendered config is written to.
const hostnameLabelRE = `[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?`

var hostnameLabel = regexp.MustCompile(`^` + hostnameLabelRE + `$`)

// validHostname reports whether h is an RFC 1123 host name: dot-separated
// labels, 253 characters at most.
func validHostname(h string) bool {
	if len(h) > 253 {
		return false
	}

	for label := range strings.SplitSeq(h, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}

	return true
}

// validAddress reports whether a is one address talosctl can reach a node at:
// an IP address, or a DNS name.
//
// A name made only of digits and dots is a mistyped IP address -- 10.0.0.256,
// 10.0.0.011 -- rather than a name, and a fully qualified name may end in a
// dot.
func validAddress(a string) bool {
	if _, err := netip.ParseAddr(a); err == nil {
		return true
	}

	name := strings.ToLower(strings.TrimSuffix(a, "."))
	if strings.Trim(name, "0123456789.") == "" {
		return false
	}

	return validHostname(name)
}

// CheckClusterName holds a cluster name to the rule Validate does, for a
// caller that has the name before it has a config: init, which takes it from
// a directory.
func CheckClusterName(name string) error {
	// It names files -- an etcd snapshot, a kubeconfig context -- so a slash
	// or a ".." would put one outside the output directory.
	if !clusterNamePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("clusterName %q may hold only letters, digits, '.', '_' and '-', starting with a "+
			"letter or digit", name)
	}

	return nil
}

// CheckHostname holds a node's hostname to the rule Validate does.
func CheckHostname(h string) error {
	// The hostname names the file the node's config is written to, so
	// anything with a slash or a ".." in it would put a file full of secrets
	// outside the gitignored output directory.
	if !validHostname(h) {
		return fmt.Errorf("hostname %q is not a valid RFC 1123 host name: lower-case letters, "+
			"digits and '-', in dot-separated labels of at most 63", h)
	}

	return nil
}

// CheckAddress holds a node's ipAddress to the rule Validate does.
func CheckAddress(a string) error {
	// talosctl splits --nodes on commas, so "a,b" would send this node's
	// config to two machines.
	if !validAddress(a) {
		return fmt.Errorf("ipAddress %q is not an IP address or a host name", a)
	}

	return nil
}

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
	} else if err := CheckClusterName(c.ClusterName); err != nil {
		add("%v", err)
	}

	c.validateEndpoint(add)
	c.validateOutputDir(add)

	if c.TalosVersion == "" {
		add("talosVersion is required: pinning it is what makes renders reproducible " +
			"(an unset version silently adopts whatever contract the local talosctl defaults to)")
	} else if !VersionPattern.MatchString(c.TalosVersion) {
		add("talosVersion %q is not a full version: installer images are tagged vMAJOR.MINOR.PATCH, "+
			"such as v1.14.1", c.TalosVersion)
	}

	// A config naming a schema talman does not know is refused rather than
	// read hopefully: the fields it does recognise may mean something else
	// there, and guessing at a machine configuration is how a cluster gets a
	// setting nobody wrote.
	switch c.APIVersion {
	case APIVersion:
	case "":
		add("apiVersion is required: add `apiVersion: %s` as the first line", APIVersion)
	default:
		add("apiVersion %q is not one this talman understands: it speaks %q", c.APIVersion, APIVersion)
	}

	if c.KubernetesVersion == "" {
		add("kubernetesVersion is required")
	} else if !VersionPattern.MatchString(c.KubernetesVersion) {
		add("kubernetesVersion %q is not a full version: Kubernetes images are tagged "+
			"vMAJOR.MINOR.PATCH, such as v1.37.1", c.KubernetesVersion)
	}

	if c.ValidationMode != "" && !slices.Contains(validValidationModes, c.ValidationMode) {
		add("validationMode %q is invalid: must be one of %s",
			c.ValidationMode, strings.Join(validValidationModes, ", "))
	}

	if c.Schematic != nil && c.SchematicID != "" {
		add("schematic and schematicID are mutually exclusive at the cluster level")
	}

	c.validateNodes(add)
	c.validatePatchKeys(add)
	c.validatePatchFiles(add)
	c.validateValuesFiles(add)
	c.validateSchematicFiles(add)
	c.validateRollout(add)

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
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() == "" {
		add("endpoint %q must be a full https URL including the port, e.g. https://10.0.0.1:6443", c.Endpoint)
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
			if err := CheckHostname(n.Hostname); err != nil {
				add("%s: %v", where, err)
			}

			if prev, dup := seenHost[n.Hostname]; dup {
				add("%s: duplicate hostname %q (also nodes[%d])", where, n.Hostname, prev)
			}

			seenHost[n.Hostname] = i
			where = "node " + n.Hostname
		}

		if n.IPAddress == "" {
			add("%s: ipAddress is required (talman passes it verbatim to talosctl --nodes)", where)
		} else if err := CheckAddress(n.IPAddress); err != nil {
			add("%s: %v", where, err)
		} else if prev, dup := seenIP[n.IPAddress]; dup {
			add("%s: duplicate ipAddress %q (also on %s)", where, n.IPAddress, prev)
		} else {
			seenIP[n.IPAddress] = where
		}

		if n.Role == "" {
			add("%s: role is required: %s or %s", where, RoleControlPlane, RoleWorker)
		}

		if n.TalosVersion != "" && !VersionPattern.MatchString(n.TalosVersion) {
			add("%s: talosVersion %q is not a full version, such as v1.14.1", where, n.TalosVersion)
		}

		seenGroup := map[string]bool{}

		for _, g := range n.Groups {
			if seenGroup[g] {
				add("%s: group %q is listed twice; its patches would be applied twice", where, g)
			}

			seenGroup[g] = true

			if g == RestWave {
				add("%s: group %q is reserved: it names the nodes no rollout wave names", where, g)
			}

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
		checkFile(add, "patches."+ref.Group, ref.Rel, ref.Path,
			"patches must be individual files so the apply order is explicit")
	}
}

// checkFile reports a path that is missing, unreadable, a directory -- with
// why that is wrong for this key -- or anything else that is not a file.
func checkFile(add func(string, ...any), where, rel, path, notDir string) {
	info, err := os.Stat(path)

	switch {
	case os.IsNotExist(err):
		add("%s: %q does not exist (resolved to %s)", where, rel, path)
	case err != nil:
		add("%s: %q is not readable: %v", where, rel, err)
	case info.IsDir():
		add("%s: %q is a directory; %s", where, rel, notDir)
	case !info.Mode().IsRegular():
		add("%s: %q is not a regular file", where, rel)
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

// validateValuesFiles checks every valuesFiles entry names a readable file, as
// validatePatchFiles does for patches.
func (c *Config) validateValuesFiles(add func(string, ...any)) {
	const notDir = "valuesFiles lists files, merged in the order given"

	for _, rel := range c.ValuesFiles {
		checkFile(add, "valuesFiles", rel, c.ResolvePath(rel), notDir)
	}

	for i := range c.Nodes {
		for _, rel := range c.Nodes[i].ValuesFiles {
			checkFile(add, "node "+c.Nodes[i].Hostname+": valuesFiles", rel, c.ResolvePath(rel), notDir)
		}
	}
}

// validateSchematicFiles checks a schematic given as a path names a readable
// file, as validatePatchFiles does for patches. Found here it is one line;
// left to rendering it is the same open error once for every node using it.
func (c *Config) validateSchematicFiles(add func(string, ...any)) {
	const notDir = "a schematic is one file"

	if c.Schematic != nil && c.Schematic.Path != "" {
		checkFile(add, "schematic", c.Schematic.Path, c.ResolvePath(c.Schematic.Path), notDir)
	}

	for i := range c.Nodes {
		if s := c.Nodes[i].Schematic; s != nil && s.Path != "" {
			checkFile(add, "node "+c.Nodes[i].Hostname+": schematic", s.Path, c.ResolvePath(s.Path), notDir)
		}
	}
}

// validateOutputDir refuses an output directory that holds the config.
//
// talman ignores the output directory whole in git, since it holds the
// rendered secrets, so one that is the cluster directory or a parent of it
// would hide talman.yaml and the encrypted bundle from git along with them.
func (c *Config) validateOutputDir(add func(string, ...any)) {
	if c.OutputDir == "" || c.Dir == "" {
		return
	}

	out := c.OutputPath()

	rel, err := filepath.Rel(out, c.Dir)
	if err != nil {
		return
	}

	if within(rel) {
		add("outputDir %q holds the config itself: talman ignores it whole in git, which would hide "+
			"talman.yaml and the bundle too; use a directory of its own, such as the default %q",
			c.OutputDir, DefaultOutputDir)

		return
	}

	// Nor may anything talman reads live in it: the bundle there would be
	// the only copy of the cluster's CAs, and git would never see it.
	inside := func(what, rel, path string) {
		if r, err := filepath.Rel(out, path); err == nil && within(r) {
			add("%s %q is inside outputDir %q, which talman ignores whole in git; keep it beside talman.yaml",
				what, rel, c.OutputDir)
		}
	}

	if c.SecretFile != "" {
		inside("secretFile", c.SecretFile, c.SecretPath())
	}

	for _, ref := range c.AllPatchPaths() {
		inside("patch", ref.Rel, ref.Path)
	}

	for _, rel := range c.ValuesFiles {
		inside("valuesFiles", rel, c.ResolvePath(rel))
	}

	if c.Schematic != nil && c.Schematic.Path != "" {
		inside("schematic", c.Schematic.Path, c.ResolvePath(c.Schematic.Path))
	}

	for i := range c.Nodes {
		n := &c.Nodes[i]

		for _, rel := range n.ValuesFiles {
			inside("node "+n.Hostname+": valuesFiles", rel, c.ResolvePath(rel))
		}

		if n.Schematic != nil && n.Schematic.Path != "" {
			inside("node "+n.Hostname+": schematic", n.Schematic.Path, c.ResolvePath(n.Schematic.Path))
		}
	}
}

// within reports whether a path relative to a directory, as filepath.Rel gives
// it, is that directory or inside it.
func within(rel string) bool {
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
