package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newNodesCmd() *cobra.Command {
	var status bool

	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List the nodes in the config",
		Long: `Nodes lists what the config says about each machine: its role, groups, how many
patches it resolves to and which Talos version it targets.

It reads the config only. --status additionally asks every node which API it
answers -- running, maintenance mode, or nothing at all -- which is how to see
what a cluster has that talman has not adopted yet.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Only with --status: without it this command touches neither the
			// talosconfig nor the cluster, so it keeps working in a checkout
			// that has no secrets bundle to generate one from.
			var (
				tal = runner(cfg)
				tc  string
			)

			if status {
				if tc, err = ensureTalosconfig(cfg); err != nil {
					return err
				}
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)

			header := "HOSTNAME\tADDRESS\tROLE\t"
			if status {
				header += "STATUS\t"
			}

			fmt.Fprintln(w, header+"GROUPS\tPATCHES\tTALOS")

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]

				groups := "-"
				if len(n.Groups) > 0 {
					groups = strings.Join(n.Groups, ",")
				}

				fmt.Fprintf(w, "%s\t%s\t%s\t", n.Hostname, n.IPAddress, n.Role)

				if status {
					fmt.Fprintf(w, "%s\t", tal.Mode(tc, n.IPAddress))
				}

				fmt.Fprintf(w, "%s\t%d\t%s\n",
					groups, len(cfg.PatchChain(n)), n.EffectiveTalosVersion(cfg))
			}

			return w.Flush()
		},
	}

	cmd.Flags().BoolVar(&status, "status", false,
		"ask each node which API it answers (running, maintenance mode, unreachable)")

	return cmd
}
