package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Work with Talos images",
	}

	cmd.AddCommand(newImageURLCmd())

	return cmd
}

func newImageURLCmd() *cobra.Command {
	var (
		nodes  []string
		submit bool
	)

	cmd := &cobra.Command{
		Use:   "url",
		Short: "Print the installer image URL for each node",
		Long: `Print the installer image reference talman passes to talosctl for each node,
rendered from imageFactory.installerURLTmpl.

This is the same value patches see as .Node.InstallerImage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printPerNode(cmd, nodes, submit, func(w *tabwriter.Writer, hostname string, ctx renderContext) {
				fmt.Fprintf(w, "%s\t%s\n", hostname, ctx.InstallerImage)
			})
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&submit, "submit", false, "register the schematic with the Image Factory first")

	return cmd
}
