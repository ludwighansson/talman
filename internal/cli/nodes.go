package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newNodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List the nodes in the config",
		Long: `Nodes lists what the config says about each machine: its role, groups, how many
patches it resolves to and which Talos version it targets.

It reads the config and nothing else, so it works in a checkout with no secrets
and no cluster to reach. "talman status" is the same table answered by the
machines themselves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)

			fmt.Fprintln(w, "HOSTNAME\tADDRESS\tROLE\tGROUPS\tPATCHES\tTALOS")

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]

				groups := "-"
				if len(n.Groups) > 0 {
					groups = strings.Join(n.Groups, ",")
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
					n.Hostname, n.IPAddress, n.Role,
					groups, len(cfg.PatchChain(n)), n.EffectiveTalosVersion(cfg))
			}

			return w.Flush()
		},
	}

	return cmd
}
