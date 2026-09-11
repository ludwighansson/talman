package cli

import (
	"github.com/spf13/cobra"
)

func newBootstrapCmd() *cobra.Command {
	var (
		node       string
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap etcd on a single control plane node",
		Long: `Bootstrap initialises etcd. It must be run exactly once, against one control
plane node, after that node's config has been applied.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			_ = cfg

			args := []string{"--talosconfig", tc, "bootstrap", "--nodes", target.IPAddress}

			return tal.Stream(append(args, extraFlags...)...)
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to bootstrap (default: the first one)")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}
