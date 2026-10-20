package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/patch"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/sopsx"
	"github.com/ludwighansson/talman/internal/template"
)

func newValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the config, the patch paths and the patch templates",
		Long: `Validate loads the config, checks that every patch key names a real group and
that every patch path resolves to a file, then renders each node's patch chain
as a template to prove it compiles and produces strategic merge patches.

It reports every problem it finds rather than stopping at the first, and needs
neither the secrets bundle nor a reachable cluster. An encrypted patch is
template-checked when sops can decrypt it, and named as unchecked when it
cannot, so validate runs without decryption keys too.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Config-level checks passed. Now prove the templates render:
			// a broken template is just as fatal as a missing file, and
			// finding out at apply time is far worse.
			var problems nodeProblems

			// One renderer for the pass: it caches schematic IDs, so a
			// per-node renderer re-reads, re-templates and re-hashes a
			// schematic file once for every node.
			r := &render.Renderer{Cfg: cfg, Submit: false}

			// Encrypted patches sops could not open, by path, reported once
			// each rather than once per node that uses them.
			undecryptable := map[string]error{}
			patches := patchReader{undecryptable: undecryptable}

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]

				ctx, err := r.Context(n)
				if err != nil {
					// Context names the node itself; the problem is what it
					// wraps.
					if inner := errors.Unwrap(err); inner != nil {
						err = inner
					}

					problems.add(n.Hostname, err)

					continue
				}

				for _, ref := range cfg.PatchChain(n) {
					raw, err := patches.read(ref)
					if err != nil {
						problems.add(n.Hostname, err)

						continue
					}

					if raw == nil {
						continue
					}

					if err := checkPatch(ref, raw, ctx); err != nil {
						problems.add(n.Hostname, err)
					}
				}
			}

			for _, rel := range slices.Sorted(maps.Keys(undecryptable)) {
				// The first line: sops follows it with every key it tried,
				// which is a lot to repeat for a note.
				why, _, _ := strings.Cut(undecryptable[rel].Error(), "\n")

				fmt.Fprintf(os.Stderr, "note: %s is encrypted and cannot be decrypted here, "+
					"so its template was not checked (%s)\n", rel, why)
			}

			// Listed as every other set of problems is, by errorText.
			if err := problems.join(len(cfg.Nodes)); err != nil {
				return err
			}

			nodes, files := len(cfg.Nodes), len(cfg.AllPatchPaths())

			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid: %d %s, %d %s\n", filepath.Base(cfg.Path),
				nodes, plural(nodes, "node", "nodes"), files, plural(files, "patch file", "patch files"))

			return nil
		},
	}

	return cmd
}

// nodeProblems collects what validate finds, node by node, and reports a
// problem once for all the nodes that share it: a broken patch in `all` is one
// problem, not one per node, and on a fifty-node cluster fifty identical lines
// would bury whatever else is wrong.
type nodeProblems struct {
	order  []string
	hosts  map[string][]string
	errors map[string]error
}

func (p *nodeProblems) add(host string, err error) {
	if p.hosts == nil {
		p.hosts = map[string][]string{}
		p.errors = map[string]error{}
	}

	text := err.Error()

	if _, seen := p.hosts[text]; !seen {
		p.order = append(p.order, text)
		p.errors[text] = err
	}

	p.hosts[text] = append(p.hosts[text], host)
}

// join is the problems as one error, each naming the nodes it was found on:
// in the order they were first found, which is config order.
func (p *nodeProblems) join(nodes int) error {
	errs := make([]error, 0, len(p.order))

	for _, text := range p.order {
		hosts := p.hosts[text]

		which := "node " + hosts[0]

		switch {
		case len(hosts) == nodes && nodes > 1:
			which = fmt.Sprintf("all %d nodes", nodes)
		case len(hosts) > 1:
			which = "nodes " + strings.Join(hosts, ", ")
		}

		errs = append(errs, fmt.Errorf("%s: %w", which, p.errors[text]))
	}

	return errors.Join(errs...)
}

// patchReader reads each patch once however many nodes use it: a patch in
// `all` on a fifty-node cluster is one file, and decrypting it fifty times
// would be fifty KMS calls, or fifty touches of a hardware key.
type patchReader struct {
	read1 map[string]patchRead
	// undecryptable is every encrypted patch this machine has no means to
	// decrypt, by the path the config wrote -- for the note at the end.
	undecryptable map[string]error
}

type patchRead struct {
	data []byte
	err  error
}

// read reads a patch, decrypting it if it is encrypted. An encrypted patch
// this machine has no means to decrypt -- no key for it, or no sops at all --
// is recorded in undecryptable and returned as nil: validate is meant to need
// no secrets, so lacking them is a limit on what it checks, not a problem in
// the config. Any other failure is one: a file the key opens but sops cannot,
// from a bad merge or a hand edit, is broken for everyone.
func (p *patchReader) read(ref config.PatchRef) ([]byte, error) {
	if p.read1 == nil {
		p.read1 = map[string]patchRead{}
	}

	got, ok := p.read1[ref.Path]
	if !ok {
		got.data, got.err = sopsx.ReadFile(ref.Path)
		p.read1[ref.Path] = got
	}

	switch err := got.err; {
	case errors.Is(err, sopsx.ErrNoKey), errors.Is(err, sopsx.ErrNotInstalled):
		p.undecryptable[ref.Rel] = err

		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", ref.Where(), err)
	}

	return got.data, nil
}

func checkPatch(ref config.PatchRef, raw []byte, ctx template.Context) error {
	rendered, err := template.Render(ref.Rel, raw, ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", ref.Where(), err)
	}

	if err := patch.CheckStrategicMerge(ref.Rel, rendered); err != nil {
		return fmt.Errorf("%s: %w", ref.Where(), err)
	}

	return nil
}
