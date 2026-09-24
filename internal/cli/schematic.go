package cli

import (
	"github.com/spf13/cobra"
)

func newSchematicCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schematic",
		Short: "Work with Image Factory schematics",
	}

	cmd.AddCommand(newSchematicIDCmd())

	return cmd
}

func newSchematicIDCmd() *cobra.Command {
	var (
		nodes  []string
		submit bool
		output outputFormat
	)

	cmd := &cobra.Command{
		Use:   "id",
		Short: "Print the Image Factory schematic ID for each node",
		Long: `Print the schematic ID each node resolves to.

The ID is the sha256 of the schematic's canonical representation, so talman
computes it offline by default and never needs to reach the factory. Pass
--submit to register the schematic with the factory and print the ID it
returns instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printPerNode(cmd, nodes, submit, output, "schematicID", func(ctx renderContext) (string, error) { return ctx.SchematicID, nil })
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	addOutputFlag(cmd, &output)
	cmd.Flags().BoolVar(&submit, "submit", false, "register the schematic with the Image Factory")

	return cmd
}
