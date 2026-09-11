package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newKubeconfigCmd() *cobra.Command {
	var (
		node       string
		force      bool
		merge      bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "kubeconfig [path]",
		Short: "Fetch the cluster kubeconfig",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			cmdArgs := []string{"--talosconfig", tc, "kubeconfig", "--nodes", target.IPAddress}

			if force {
				cmdArgs = append(cmdArgs, "--force")
			}

			cmdArgs = append(cmdArgs, fmt.Sprintf("--merge=%t", merge))
			cmdArgs = append(cmdArgs, extraFlags...)
			cmdArgs = append(cmdArgs, args...)

			return tal.Stream(cmdArgs...)
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to fetch from (default: the first one)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing context")
	cmd.Flags().BoolVar(&merge, "merge", true, "merge into the existing kubeconfig")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}
