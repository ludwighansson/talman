package cli

import (
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/factory"
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
		kind   string
		arch   string
		format string
		output outputFormat
	)

	cmd := &cobra.Command{
		Use:   "url",
		Short: "Print the installer image, or a boot medium's URL, for each node",
		Long: `Print the installer image reference talman passes to talosctl for each node,
rendered from imageFactory.installerURLTmpl -- the same value patches see as
.Node.InstallerImage.

--kind names a boot medium for the same schematic and Talos version instead,
for bringing up a machine that has no Talos on it yet:

  iso    the ISO to boot from
  disk   the platform's disk image (--format overrides its extension)
  pxe    the iPXE script that netboots it

They come from the Image Factory the config names, for the node's platform,
with the secure boot variant when secureBoot is set. --arch picks the machine
architecture, amd64 by default.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			field := "installerImage"
			if kind != "installer" {
				field = kind
			}

			return printPerNode(cmd, nodes, submit, output, field, func(ctx renderContext) (string, error) {
				if kind == "installer" {
					return ctx.InstallerImage, nil
				}

				return ctx.Factory.ImageURL(kind, ctx.SchematicID, ctx.TalosVersion, arch, format)
			})
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	addOutputFlag(cmd, &output)
	cmd.Flags().StringVar(&kind, "kind", "installer", "what to print: installer, iso, disk or pxe")
	cmd.Flags().StringVar(&arch, "arch", "amd64", "machine architecture for a boot medium: amd64 or arm64")
	cmd.Flags().StringVar(&format, "format", "", "the disk image's extension, for --kind disk (default: the platform's)")

	cmd.PreRunE = func(*cobra.Command, []string) error {
		if !slices.Contains([]string{"installer", factory.KindISO, factory.KindDisk, factory.KindPXE}, kind) {
			return fmt.Errorf("--kind %q is invalid: must be installer, iso, disk or pxe", kind)
		}

		if !slices.Contains([]string{"amd64", "arm64"}, arch) {
			return fmt.Errorf("--arch %q is invalid: must be amd64 or arm64", arch)
		}

		if format != "" && kind != factory.KindDisk {
			return errors.New("--format applies to --kind disk only")
		}

		return nil
	}
	cmd.Flags().BoolVar(&submit, "submit", false, "register the schematic with the Image Factory first")

	return cmd
}
