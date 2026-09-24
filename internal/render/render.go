// Package render turns a talman config plus a pile of patch files into
// complete Talos machine configurations.
package render

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/factory"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/patch"
	"github.com/ludwighansson/talman/internal/redact"
	"github.com/ludwighansson/talman/internal/sopsx"
	"github.com/ludwighansson/talman/internal/talosctl"
	"github.com/ludwighansson/talman/internal/template"
)

// Renderer holds everything a render pass needs.
type Renderer struct {
	Cfg *config.Config
	Tal *talosctl.Runner

	// Submit POSTs each distinct schematic to the Image Factory and uses the
	// ID it returns, instead of computing the ID locally.
	Submit bool
	// Log receives progress lines. Nil discards them.
	Log io.Writer
	// ExtraArgs are appended verbatim to each `talosctl gen config` call.
	ExtraArgs []string

	// workspace is a 0700 temp dir holding the decrypted secrets bundle and
	// the rendered patches for the duration of a pass.
	workspace   string
	secretsFile string
	// unregister drops the workspace from the forced-exit cleanup list once
	// Close has removed it.
	unregister func()

	// secrets is every secret value the pass has rendered into a config: the
	// bundle, the values SOPS decrypted out of patches, and whatever a
	// template read from the environment. secretsErr is why that list may be
	// incomplete, which a caller about to print a config has to know.
	secrets    *redact.Set
	secretsMu  sync.Mutex
	secretsErr error

	// schematicIDs caches resolved schematics for the pass, so a file shared
	// by fifty nodes is read, templated and hashed once.
	schematicIDs map[string]string

	// One Renderer serves a whole pass, and a pass may render nodes
	// concurrently. Everything per node is already separate -- each writes
	// its patches under its own hostname -- so these two are the whole of the
	// sharing: the cache, and the log nobody wants interleaved mid-line.
	schemaMu sync.Mutex
	logMu    sync.Mutex
}

// Result is one node's rendered machine config.
type Result struct {
	Node    *config.Node
	Path    string
	Content []byte
	Chain   []config.PatchRef
	Context template.Context
}

func (r *Renderer) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}

	r.logMu.Lock()
	defer r.logMu.Unlock()

	fmt.Fprintf(r.Log, format+"\n", args...)
}

// Open prepares the workspace and decrypts the secrets bundle into it.
//
// talosctl needs a filesystem path for --with-secrets, so the plaintext bundle
// is written to a 0700 temp dir and removed by Close. It never lands in the
// output directory or the repo.
func (r *Renderer) Open() error {
	if err := r.Tal.Ensure(); err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "talman-")
	if err != nil {
		return err
	}

	r.workspace = dir
	r.unregister = interrupt.RemoveAllOnExit(dir)
	r.schematicIDs = map[string]string{}

	secretPath := r.Cfg.SecretPath()

	if _, err := os.Stat(secretPath); err != nil {
		r.Close()

		return fmt.Errorf("secrets bundle %s is missing: run `talman secrets generate` first "+
			"(regenerating secrets for a live cluster orphans it, so talman will not create one implicitly)",
			r.Cfg.SecretFile)
	}

	plaintext, err := sopsx.ReadFile(secretPath)
	if err != nil {
		r.Close()

		return err
	}

	r.secrets = redact.NewSet()

	if err := r.secrets.AddBundle(plaintext); err != nil {
		r.noteSecretsErr(fmt.Errorf("%s: %w", r.Cfg.SecretFile, err))
	}

	r.secretsFile = filepath.Join(dir, "secrets.yaml")

	if err := os.WriteFile(r.secretsFile, plaintext, 0o600); err != nil {
		r.Close()

		return err
	}

	return nil
}

// Secrets returns a redactor for every secret this pass has rendered so far,
// or an error saying why talman cannot be sure it knows them all.
//
// Built from what the pass already decrypted: the bundle was read once, in
// Open, and reading it again to hide what it holds would cost a second sops
// run -- a second KMS call, or a second touch of a hardware key.
func (r *Renderer) Secrets() (*redact.Redactor, error) {
	r.secretsMu.Lock()
	err := r.secretsErr
	r.secretsMu.Unlock()

	if err != nil {
		return nil, err
	}

	if r.secrets == nil {
		return nil, errors.New("renderer used before Open: this is a talman bug, please report it")
	}

	return r.secrets.Redactor(), nil
}

func (r *Renderer) noteSecretsErr(err error) {
	r.secretsMu.Lock()
	defer r.secretsMu.Unlock()

	if r.secretsErr == nil {
		r.secretsErr = err
	}
}

// Close removes the workspace and the decrypted secrets inside it.
func (r *Renderer) Close() {
	if r.workspace != "" {
		_ = os.RemoveAll(r.workspace)
		r.workspace = ""
		r.unregister()
	}
}

// Node renders one node's machine config in memory.
func (r *Renderer) Node(n *config.Node) (*Result, error) {
	ctx, err := r.Context(n)
	if err != nil {
		return nil, err
	}

	// Without Open, r.workspace is "" and the patch directory below would
	// resolve relative to the caller's working directory -- writing rendered
	// patches, which may hold secrets decrypted out of SOPS, into the
	// operator's config repo. Context() is deliberately usable un-Opened
	// (validate and the schematic commands rely on it), so Node() has to say
	// so itself.
	if r.workspace == "" {
		return nil, errors.New("renderer used before Open: this is a talman bug, please report it")
	}

	chain := r.Cfg.PatchChain(n)

	patchDir := filepath.Join(r.workspace, "patches", n.Hostname)
	if err := os.MkdirAll(patchDir, 0o700); err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(chain))

	for i, ref := range chain {
		rendered, err := r.renderPatch(ref, ctx)
		if err != nil {
			return nil, fmt.Errorf("node %s: %w", n.Hostname, err)
		}

		// Skip files that render to nothing: a patch wrapped entirely in a
		// conditional is a normal idiom, and talosctl rejects an empty patch.
		if len(strings.TrimSpace(string(rendered))) == 0 {
			r.logf("  %-14s %s (skipped: renders empty)", "["+ref.Group+"]", ref.Rel)

			continue
		}

		out := filepath.Join(patchDir, fmt.Sprintf("%03d-%s", i, safeName(ref.Rel)))

		if err := os.WriteFile(out, rendered, 0o600); err != nil {
			return nil, err
		}

		paths = append(paths, out)
	}

	outputType := "worker"
	if n.IsControlPlane() {
		outputType = "controlplane"
	}

	gen := func(with []string) ([]byte, error) {
		return r.Tal.GenConfig(talosctl.GenConfigOptions{
			ClusterName:       r.Cfg.ClusterName,
			Endpoint:          r.Cfg.Endpoint,
			SecretsFile:       r.secretsFile,
			TalosVersion:      n.EffectiveTalosVersion(r.Cfg),
			KubernetesVersion: r.Cfg.KubernetesVersion,
			InstallImage:      ctx.Node.InstallerImage,
			OutputType:        outputType,
			Patches:           with,
			ExtraArgs:         r.ExtraArgs,
		})
	}

	content, err := gen(paths)
	if err != nil {
		return nil, fmt.Errorf("node %s: %s", n.Hostname, r.blame(err, gen, chain, paths))
	}

	return &Result{
		Node:    n,
		Path:    r.Cfg.MachineConfigPath(n),
		Content: content,
		Chain:   chain,
		Context: ctx,
	}, nil
}

// renderPatch reads, templates and sanity-checks a single patch file.
func (r *Renderer) renderPatch(ref config.PatchRef, ctx template.Context) ([]byte, error) {
	// Read through sops so an individual patch holding a registry credential
	// or a SideroLink token can be encrypted too, not just the secrets bundle.
	raw, ciphertext, err := sopsx.Read(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	// What SOPS decrypted is what the operator marked secret, and it is about
	// to be rendered into a machine config.
	if ciphertext != nil {
		if err := r.secrets.AddEncrypted(ciphertext, raw); err != nil {
			r.noteSecretsErr(fmt.Errorf("%s: cannot tell which values are encrypted: %w", ref.Rel, err))
		}
	}

	rendered, err := template.RenderSeeing(ref.Rel, raw, ctx, func(v string) { r.secrets.Add(v) })
	if err != nil {
		return nil, fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	if err := patch.CheckStrategicMerge(ref.Rel, rendered); err != nil {
		return nil, fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	return rendered, nil
}

// Context builds the template scope for a node, resolving its schematic and
// installer image first so patches can reference them.
func (r *Renderer) Context(n *config.Node) (template.Context, error) {
	base := template.Context{
		Cluster: template.Cluster{
			Name:              r.Cfg.ClusterName,
			Endpoint:          r.Cfg.Endpoint,
			TalosVersion:      r.Cfg.TalosVersion,
			KubernetesVersion: r.Cfg.KubernetesVersion,
		},
		Node: template.Node{
			Hostname:     n.Hostname,
			IPAddress:    n.IPAddress,
			Role:         string(n.Role),
			Groups:       n.Groups,
			Values:       n.Values,
			TalosVersion: n.EffectiveTalosVersion(r.Cfg),
		},
		Values: r.Cfg.Values,
	}

	id, err := r.schematicID(n, base)
	if err != nil {
		return base, fmt.Errorf("node %s: %w", n.Hostname, err)
	}

	installer, err := r.Cfg.ImageFactoryFor(n).InstallerURL(id, n.EffectiveTalosVersion(r.Cfg))
	if err != nil {
		return base, fmt.Errorf("node %s: %w", n.Hostname, err)
	}

	base.Node.SchematicID = id
	base.Node.InstallerImage = installer

	return base, nil
}

// schematicID resolves the node's schematic to an ID.
//
// A schematic given as a file path is itself templated, but with a context in
// which SchematicID and InstallerImage are still empty -- the ID cannot depend
// on itself. Everything else (.Values, .Node.Values) is available.
func (r *Renderer) schematicID(n *config.Node, base template.Context) (string, error) {
	if n.SchematicID != "" {
		return n.SchematicID, nil
	}

	ref := n.Schematic
	if ref == nil {
		if r.Cfg.SchematicID != "" {
			return r.Cfg.SchematicID, nil
		}

		ref = r.Cfg.Schematic
	}

	if ref == nil {
		return factory.VanillaID, nil
	}

	schematic, err := r.loadSchematic(ref, base)
	if err != nil {
		return "", err
	}

	canonical, err := schematic.Marshal()
	if err != nil {
		return "", err
	}

	// Context is reachable without Open (validate does exactly that), so the
	// cache is created on demand rather than assumed.
	//
	// The lock covers the cache, not the work below it: submitting the same
	// schematic twice in a race returns the same ID from the Image Factory,
	// which is a far better trade than holding a mutex across a network call
	// while every other node waits.
	r.schemaMu.Lock()

	if r.schematicIDs == nil {
		r.schematicIDs = map[string]string{}
	}

	cached, ok := r.schematicIDs[string(canonical)]

	r.schemaMu.Unlock()

	if ok {
		return cached, nil
	}

	var id string

	if r.Submit {
		if id, err = r.Cfg.ImageFactoryFor(n).Submit(schematic); err != nil {
			return "", err
		}

		if local, lerr := schematic.ID(); lerr == nil && local != id {
			// Straight to stderr, not through logf: Log is nil for every
			// --submit caller except render, and this is the one diagnostic
			// that reveals talman's offline ID computation disagreeing with
			// the factory. Silently dropping it defeats its purpose.
			fmt.Fprintf(os.Stderr, "warning: factory returned schematic %s but talman computed %s; "+
				"the factory canonicalised a field talman does not model\n", id, local)
		}
	} else if id, err = schematic.ID(); err != nil {
		return "", err
	}

	r.schemaMu.Lock()
	r.schematicIDs[string(canonical)] = id
	r.schemaMu.Unlock()

	return id, nil
}

func (r *Renderer) loadSchematic(ref *config.SchematicRef, base template.Context) (*factory.Schematic, error) {
	if ref.Inline != nil {
		return ref.Inline, nil
	}

	path := ref.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.Cfg.Dir, path)
	}

	raw, err := sopsx.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("schematic %q: %w", ref.Path, err)
	}

	rendered, err := template.Render(ref.Path, raw, base)
	if err != nil {
		return nil, fmt.Errorf("schematic %w", err)
	}

	schematic, err := factory.Unmarshal(rendered)
	if err != nil {
		return nil, fmt.Errorf("schematic %q: %w", ref.Path, err)
	}

	return schematic, nil
}

// Secrets returns a redactor for the secrets a config's machine configs may
// hold, without rendering them: the bundle and the values SOPS encrypts in
// its patches.
//
// For a caller printing configs that were rendered earlier, such as `apply
// --no-render`. What a template read from the environment at that render is
// beyond it, because nothing recorded it.
func Secrets(cfg *config.Config) (*redact.Redactor, error) {
	s := redact.NewSet()

	plaintext, err := sopsx.ReadFile(cfg.SecretPath())
	if err != nil {
		return nil, err
	}

	if err := s.AddBundle(plaintext); err != nil {
		return nil, fmt.Errorf("%s: %w", cfg.SecretFile, err)
	}

	for _, ref := range cfg.AllPatchPaths() {
		plaintext, ciphertext, err := sopsx.Read(ref.Path)
		if err != nil {
			return nil, err
		}

		if ciphertext == nil {
			continue
		}

		if err := s.AddEncrypted(ciphertext, plaintext); err != nil {
			return nil, fmt.Errorf("%s: cannot tell which values are encrypted: %w", ref.Rel, err)
		}
	}

	return s.Redactor(), nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// safeName turns a patch's config-relative path into a flat filename that
// still shows where it came from, so a talosctl error naming the temp file is
// traceable back to the source patch.
func safeName(rel string) string {
	name := unsafeName.ReplaceAllString(strings.TrimPrefix(rel, "./"), "_")

	name = strings.Trim(name, "_")
	if name == "" {
		name = "patch.yaml"
	}

	if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
		name += ".yaml"
	}

	return name
}

// chainIndex recovers the patch-chain position from a written patch filename.
// Patches that render empty are skipped, so the indices of the written files
// and of the chain do not line up; the filename carries the chain index.
func chainIndex(path string) (int, bool) {
	var idx int

	if _, err := fmt.Sscanf(filepath.Base(path), "%03d-", &idx); err != nil {
		return 0, false
	}

	return idx, true
}

// blame turns a talosctl rejection into a message naming the patch file
// responsible.
//
// talosctl reports the offending *document* ("error decoding document
// v1alpha1/SysctlConfig/"), not the file it came from, and by the time the
// error surfaces the temp file is gone. So on failure talman re-runs the
// generation with growing prefixes of the chain and reports the first patch
// that breaks it.
//
// Growing prefixes rather than testing each patch alone is deliberate: some
// Talos v1.14 document pairs are mutually exclusive, so a patch can be valid
// by itself and still be the one that makes the config unacceptable.
func (r *Renderer) blame(cause error, gen func([]string) ([]byte, error),
	chain []config.PatchRef, paths []string,
) string {
	msg := cause.Error()

	// An interrupted generation was not rejected, and every re-run would fail
	// the same way and blame whatever came first.
	if len(paths) == 0 || interrupt.Interrupted() {
		return msg
	}

	// If the base config alone fails, no patch is at fault.
	if _, err := gen(nil); err != nil {
		return msg + "\n  (the generated base config is already invalid; no patch is at fault)"
	}

	for i := range paths {
		if _, err := gen(paths[:i+1]); err == nil {
			continue
		}

		ref := config.PatchRef{Rel: filepath.Base(paths[i])}
		pos := i

		if idx, ok := chainIndex(paths[i]); ok && idx < len(chain) {
			ref = chain[idx]
			pos = idx
		}

		// pos is the position in the chain the operator sees from
		// `talman patches`, not the index among the files actually written:
		// patches that render empty are skipped, so the two diverge and
		// quoting the wrong one contradicts the listing.
		hint := ""
		if pos > 0 {
			hint = fmt.Sprintf("\n  (it is the %d%s patch in the chain `talman patches` lists; "+
				"the ones before it applied cleanly)", pos+1, ordinal(pos+1))
		}

		return fmt.Sprintf("%s\n  rejected by: patches.%s -> %s%s", msg, ref.Group, ref.Rel, hint)
	}

	return msg
}

func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}

	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}
