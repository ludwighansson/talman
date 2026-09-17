package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
)

func newRenderCmd() *cobra.Command {
	var (
		nodes         []string
		submit        bool
		noValidate    bool
		dryRun        bool
		toStdout      bool
		noTalosconfig bool
		parallel      int
		extraFlags    []string
	)

	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render machine configurations for every node",
		Long: `Render runs one "talosctl gen config" per node with that node's resolved patch
chain, writing complete machine configurations to the output directory.

The secrets bundle is decrypted into a private temporary directory for the
duration of the run and removed afterwards; plaintext secrets never reach the
output directory or the repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			r, err := newRenderer(cfg, submit, os.Stderr)
			if err != nil {
				return err
			}
			defer r.Close()

			// Forwarded to the per-node `talosctl gen config` calls, which is
			// the invocation this command exists to drive.
			r.ExtraArgs = extraFlags

			// Two talosctl processes per node -- gen config, then validate --
			// and nothing between nodes depends on anything else, so a large
			// cluster has no reason to render one node at a time.
			results, err := eachNode(targets, parallel, func(n *config.Node) (*render.Result, error) {
				res, err := r.Node(n)
				if err != nil {
					return nil, err
				}

				if !noValidate {
					if err := r.Validate(res, cfg.TalosMode); err != nil {
						return nil, err
					}
				}

				return res, nil
			})
			if err != nil {
				return err
			}

			switch {
			case toStdout:
				// A bare "---" would be indistinguishable from the document
				// separators inside each machine config, leaving a reader
				// unable to tell where one node ends and the next begins. A
				// comment banner at column 0 cannot occur in generated
				// output, so boundaries stay recoverable.
				for _, res := range results {
					fmt.Fprintf(cmd.OutOrStdout(), "# talman: %s\n%s", res.Node.Hostname, res.Content)
				}

				return nil
			case dryRun:
				for _, res := range results {
					fmt.Fprintf(cmd.OutOrStdout(), "would write %s (%d patches)\n",
						render.Rel(res.Path), len(res.Chain))
				}

				return nil
			default:
				return r.WriteAll(results, !noTalosconfig)
			}
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil,
		"render only these nodes (hostname or IP; repeatable)")
	cmd.Flags().BoolVar(&submit, "submit", false,
		"POST schematics to the Image Factory and use the returned IDs instead of computing them locally")
	cmd.Flags().BoolVar(&noValidate, "no-validate", false,
		"skip talosctl validate on each rendered config")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"render and validate, but write nothing")
	cmd.Flags().BoolVar(&toStdout, "stdout", false,
		"write rendered configs to stdout, each preceded by a \"# talman: <hostname>\" banner")
	cmd.Flags().BoolVar(&noTalosconfig, "no-talosconfig", false,
		"do not generate a talosconfig")
	addParallelFlag(cmd, &parallel, defaultParallel, "how many nodes to render at once")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}
