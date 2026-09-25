package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// gitignoreHeader marks a .gitignore as talman's, and so one it may rewrite.
const gitignoreHeader = "# Managed by talman.\n"

// gitignoreBody keeps rendered output out of git wholesale.
//
// A rendered machine config embeds the machine CA key, the cluster secret and
// the bootstrap token, so the safe default is to ignore the directory rather
// than to list files as they appear -- a new node must not be able to slip
// into a commit because nobody re-ran the generator.
const gitignoreBody = gitignoreHeader + `# Rendered machine configs and the talosconfig contain live cluster secrets.
*
!.gitignore
`

// WriteAll writes every rendered config, the talosconfig, and the .gitignore.
func (r *Renderer) WriteAll(results []*Result, writeTalosconfig bool) error {
	if err := r.prepareOutput(); err != nil {
		return err
	}

	for _, res := range results {
		if err := writeAtomic(res.Path, res.Content); err != nil {
			return fmt.Errorf("writing %s: %w", res.Path, err)
		}

		r.logf("wrote %s", Rel(res.Path))
	}

	if !writeTalosconfig {
		return nil
	}

	path, err := r.WriteTalosconfig()
	if err != nil {
		return err
	}

	r.logf("wrote %s", Rel(path))

	return nil
}

// PrepareOutput creates the output directory and the .gitignore guarding it,
// for callers that put a file there without rendering anything -- the fetched
// kubeconfig is as much a cluster credential as the configs beside it.
func PrepareOutput(cfg *config.Config, log io.Writer) error {
	r := &Renderer{Cfg: cfg, Log: log}

	return r.prepareOutput()
}

// prepareOutput creates the output directory and the .gitignore guarding it.
//
// Separate from WriteAll because the talosconfig can be the first thing ever
// written to a cluster directory: commands that only need the credential
// generate it on its own, and it carries the same cluster PKI as the machine
// configs beside it. The directory it lands in has to be ignored either way.
func (r *Renderer) prepareOutput() error {
	dir := r.Cfg.OutputPath()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	return r.ensureGitignore(dir)
}

func (r *Renderer) ensureGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")

	existing, err := os.ReadFile(path)
	if err == nil {
		if ignoresEverything(existing) {
			return nil
		}

		// Someone else's: the output directory is shared with something that
		// keeps its own .gitignore. Replacing it destroys what they had, and
		// keeping it leaves the secrets committable, so neither is talman's
		// call to make.
		if !strings.HasPrefix(string(existing), gitignoreHeader) {
			return fmt.Errorf("%s exists, was not written by talman, and does not ignore the directory, "+
				"where rendered configs hold live cluster secrets: point outputDir at a directory of its own",
				Rel(path))
		}

		// talman's, but no longer doing its job -- truncated, or edited.
		// Since what sits beside it is machine CA keys and the bootstrap
		// token, restoring the rule beats respecting the edit.
		r.logf("warning: %s does not ignore the directory; rewriting it, "+
			"because the configs beside it contain live cluster secrets", Rel(path))
	} else if !os.IsNotExist(err) {
		return err
	}

	return os.WriteFile(path, []byte(gitignoreBody), 0o644)
}

// ignoresEverything reports whether a .gitignore excludes the whole directory,
// which is the only pattern that keeps a node added later from being
// committable before anyone re-runs the generator.
func ignoresEverything(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "*" {
			return true
		}
	}

	return false
}

// WriteTalosconfig generates the client config and points it at the cluster.
//
// `talosctl gen config` emits `endpoints: []` -- the Go option that sets them
// has no CLI flag -- so the endpoint and node lists are a required second
// step, done with `talosctl config endpoint|node` rather than by editing YAML
// ourselves.
func (r *Renderer) WriteTalosconfig() (string, error) {
	if err := r.prepareOutput(); err != nil {
		return "", err
	}

	content, err := r.Tal.GenConfig(talosctl.GenConfigOptions{
		ClusterName:  r.Cfg.ClusterName,
		Endpoint:     r.Cfg.Endpoint,
		SecretsFile:  r.secretsFile,
		TalosVersion: r.Cfg.TalosVersion,
		OutputType:   "talosconfig",
	})
	if err != nil {
		return "", err
	}

	// Build it in the workspace so a failure halfway through cannot leave a
	// talosconfig with no endpoints where a working one used to be.
	staging := filepath.Join(r.workspace, "talosconfig")

	if err := os.WriteFile(staging, content, 0o600); err != nil {
		return "", err
	}

	if eps := r.endpoints(); len(eps) > 0 {
		if err := r.Tal.ConfigEndpoint(staging, eps); err != nil {
			return "", err
		}
	}

	if nodes := r.nodeAddrs(); len(nodes) > 0 {
		if err := r.Tal.ConfigNode(staging, nodes); err != nil {
			return "", err
		}
	}

	final, err := os.ReadFile(staging)
	if err != nil {
		return "", err
	}

	path := r.Cfg.TalosconfigPath()

	if err := writeAtomic(path, final); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	return path, nil
}

// WriteAtomic replaces path in one step, for callers outside this package --
// the secrets bundle above all, whose truncation orphans a live cluster.
func WriteAtomic(path string, content []byte) error {
	return writeAtomic(path, content)
}

// writeAtomic replaces path in one step, via a temporary file in the same
// directory.
//
// os.WriteFile truncates first, so a full disk or an interrupt between
// truncate and write leaves a half-written machine config -- or a talosconfig
// with no endpoints -- where the operator's working one used to be. The temp
// file is created 0600 and renamed, so the destination is never observable in
// a partial state and never briefly world-readable.
func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()

	defer func() {
		tmp.Close()        //nolint:errcheck // best effort; the rename below is what matters
		os.Remove(tmpName) //nolint:errcheck // no-op once the rename has succeeded
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return err
	}

	if _, err := tmp.Write(content); err != nil {
		return err
	}

	// Durability before visibility: a rename that outlives its own contents
	// across a crash would defeat the point.
	if err := tmp.Sync(); err != nil {
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, path)
}

// endpoints are the control plane addresses: the Talos API is only served
// with full functionality there.
func (r *Renderer) endpoints() []string {
	cps := r.Cfg.ControlPlanes()

	out := make([]string, 0, len(cps))

	for _, n := range cps {
		out = append(out, n.IPAddress)
	}

	return out
}

func (r *Renderer) nodeAddrs() []string {
	out := make([]string, 0, len(r.Cfg.Nodes))

	for i := range r.Cfg.Nodes {
		out = append(out, r.Cfg.Nodes[i].IPAddress)
	}

	return out
}

// Validate runs `talosctl validate` over rendered content by staging it in the
// workspace, so validation never depends on the output having been written.
func (r *Renderer) Validate(res *Result, mode string) error {
	staging := filepath.Join(r.workspace, "validate-"+res.Node.Hostname+".yaml")

	if err := os.WriteFile(staging, res.Content, 0o600); err != nil {
		return err
	}

	if err := r.Tal.Validate(staging, mode); err != nil {
		// Validation quotes the config it rejects, as generation does.
		return r.redacted(fmt.Errorf("node %s: %w", res.Node.Hostname, err))
	}

	return nil
}

// Nodes selects the nodes to operate on: all of them, or the named subset.
// Names match a hostname first, then an IP address.
func Nodes(cfg *config.Config, names []string) ([]*config.Node, error) {
	if len(names) == 0 {
		out := make([]*config.Node, 0, len(cfg.Nodes))
		for i := range cfg.Nodes {
			out = append(out, &cfg.Nodes[i])
		}

		return out, nil
	}

	var out []*config.Node

	// Deduplicated: a node can be named twice, or once by hostname and once
	// by address. These selections feed apply, upgrade and reset, where a
	// duplicate means resetting or upgrading the same machine twice off a
	// single confirmation.
	seen := make(map[string]bool, len(names))

	for _, name := range names {
		n, ok := cfg.Node(name)
		if !ok {
			return nil, fmt.Errorf("no node %q in %s: known nodes are %s",
				name, Rel(cfg.Path), hostnames(cfg))
		}

		if seen[n.Hostname] {
			continue
		}

		seen[n.Hostname] = true

		out = append(out, n)
	}

	return out, nil
}

func hostnames(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		names = append(names, cfg.Nodes[i].Hostname)
	}

	return strings.Join(names, ", ")
}

// Rel shortens a path against the working directory for display.
func Rel(path string) string {
	wd, err := os.Getwd()
	if err != nil {
		return path
	}

	r, err := filepath.Rel(wd, path)
	if err != nil || len(r) > len(path) {
		return path
	}

	return r
}
