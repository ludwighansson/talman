package cli

import (
	"fmt"
	"os"
	"path/filepath"

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
neither the secrets bundle nor a reachable cluster.`,
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

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]

				ctx, err := r.Context(n)
				if err != nil {
					problems = append(problems, err)

					continue
				}

				for _, ref := range cfg.PatchChain(n) {
					if err := checkPatch(ref, ctx); err != nil {
						problems = append(problems, fmt.Errorf("node %s: %w", n.Hostname, err))
					}
				}
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

func checkPatch(ref config.PatchRef, ctx template.Context) error {
	raw, err := sopsx.ReadFile(ref.Path)
	if err != nil {
		return fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	rendered, err := template.Render(ref.Rel, raw, ctx)
	if err != nil {
		return fmt.Errorf("patches.%s: %w", ref.Group, err)
	}

	return patch.CheckStrategicMerge(ref.Rel, rendered)
}

func countPatches(cfg *config.Config) int {
	return len(cfg.AllPatchPaths())
}
