package cli

import (
	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/render"
)

func newDashboardCmd() *cobra.Command {
	var extraFlags []string

	cmd := &cobra.Command{
		Use:   "dashboard <node>",
		Short: "Open the Talos dashboard for one node",
		Long: `Dashboard opens "talosctl dashboard": a text UI showing one node's overview,
its logs and live metrics.

The node is named as a hostname or an address from the config, and exactly one
is required -- a dashboard is a view of a machine, not of a cluster.

talman reaches it at its own address rather than through the talosconfig
endpoints. A dashboard is most wanted when the cluster is unhappy, which is
when a control plane proxying for the node is least able to serve it.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeNodes,
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			// Named rather than resolved by hand: this is the lookup that
			// reports unknown names with the list of known ones.
			targets, err := render.Nodes(cfg, args)
			if err != nil {
				return err
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			n := targets[0]

			cmdArgs := make([]string, 0, 7+len(extraFlags))
			cmdArgs = append(cmdArgs,
				"--talosconfig", tc,
				"--endpoints", n.IPAddress,
				"--nodes", n.IPAddress,
				"dashboard",
			)

			return runner(cfg).Stream(append(cmdArgs, extraFlags...)...)
		},
	}

	addExtraFlags(cmd, &extraFlags)

	return cmd
}
