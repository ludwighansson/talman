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
			var problems []error

			// One renderer for the pass: it caches schematic IDs, so a
			// per-node renderer re-reads, re-templates and re-hashes a
			// schematic file once for every node.
			r := &render.Renderer{Cfg: cfg, Submit: false}

			// Encrypted patches sops could not open, by path, reported once
			// each rather than once per node that uses them.
			undecryptable := map[string]error{}

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]

				ctx, err := r.Context(n)
				if err != nil {
					problems = append(problems, err)

					continue
				}

				for _, ref := range cfg.PatchChain(n) {
					raw, err := readPatch(ref, undecryptable)
					if err != nil {
						problems = append(problems, fmt.Errorf("node %s: %w", n.Hostname, err))

						continue
					}

					if raw == nil {
						continue
					}

					if err := checkPatch(ref, raw, ctx); err != nil {
						problems = append(problems, fmt.Errorf("node %s: %w", n.Hostname, err))
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

			if len(problems) > 0 {
				for _, p := range problems {
					fmt.Fprintln(os.Stderr, "  "+p.Error())
				}

				return fmt.Errorf("%d problem(s) in %s", len(problems), filepath.Base(cfg.Path))
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%s is valid: %d nodes, %d patch files\n",
				filepath.Base(cfg.Path), len(cfg.Nodes), countPatches(cfg))

			return nil
		},
	}

	return cmd
}

// readPatch reads a patch, decrypting it if it is encrypted. An encrypted
// patch this machine has no means to decrypt -- no key for it, or no sops at
// all -- is recorded in undecryptable and returned as nil: validate promises
// to need no secrets, so lacking them is a limit on what it checks, not a
// problem in the config. Any other failure is one: a file the key opens but
// sops cannot, from a bad merge or a hand edit, is broken for everyone.
func readPatch(ref config.PatchRef, undecryptable map[string]error) ([]byte, error) {
	if _, seen := undecryptable[ref.Rel]; seen {
		return nil, nil
	}

	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	if !sopsx.IsEncrypted(data) {
		return data, nil
	}

	plaintext, err := sopsx.ReadFile(ref.Path)

	switch {
	case errors.Is(err, sopsx.ErrNoKey), errors.Is(err, sopsx.ErrNotInstalled):
		undecryptable[ref.Rel] = err

		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	return plaintext, nil
}

func checkPatch(ref config.PatchRef, raw []byte, ctx template.Context) error {
	rendered, err := template.Render(ref.Rel, raw, ctx)
	if err != nil {
		return fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	return patch.CheckStrategicMerge(ref.Rel, rendered)
}

func countPatches(cfg *config.Config) int {
	return len(cfg.AllPatchPaths())
}
