package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/render"
)

func newPatchesCmd() *cobra.Command {
	var nodes []string

	cmd := &cobra.Command{
		Use:   "patches",
		Short: "Show the resolved patch chain for each node",
		Long: `Patches prints, in application order, the patch files each node receives and
the key each one came from.

Talos applies strategic merge patches in sequence and the last writer wins, so
this listing is the authoritative answer to "why does this node have that
value".`,
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

			out := cmd.OutOrStdout()

			for i, n := range targets {
				if i > 0 {
					fmt.Fprintln(out)
				}

				fmt.Fprintf(out, "%s (%s", n.Hostname, n.Role)

				if len(n.Groups) > 0 {
					fmt.Fprintf(out, ", groups: %s", strings.Join(n.Groups, ", "))
				}

				fmt.Fprintln(out, ")")

				chain := cfg.PatchChain(n)
				if len(chain) == 0 {
					fmt.Fprintln(out, "  (no patches)")

					continue
				}

				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

				for j, ref := range chain {
					fmt.Fprintf(w, "  %d.\t[%s]\t%s\n", j+1, ref.Group, ref.Rel)
				}

				_ = w.Flush()
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")

	return cmd
}
