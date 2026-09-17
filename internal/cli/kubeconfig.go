package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
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
		Short: "Fetch the cluster kubeconfig into the output directory",
		Long: `Kubeconfig downloads the admin kubeconfig and writes it to
clusterconfig/kubeconfig, beside the machine configs and the talosconfig.

It never touches ~/.kube/config unless you name it. Left to itself talosctl
merges into whatever kubeconfig the environment happens to point at, which for
a tool that manages several clusters means one cluster's admin credentials
landing in another cluster's file. Name a path to choose the destination, and
"-" to write to stdout.

A path you name is merged into, as talosctl would; talman's own copy is
overwritten, because it is a generated artefact of this cluster directory.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			// No path: talman's own copy, which it may overwrite freely.
			dest := cfg.KubeconfigPath()
			own := len(args) == 0

			if !own {
				dest = args[0]
			}

			// Merging is right for a file the operator names -- usually
			// ~/.kube/config, where the point is to add a context. It is wrong
			// for talman's own copy twice over: there is nothing to preserve,
			// and merging the same context twice fails on the second run
			// unless --force. It is also how talosctl distinguishes a write to
			// stdout, which it only does when merging is off. An explicit
			// --merge still wins.
			if (own || dest == "-") && !cmd.Flags().Changed("merge") {
				merge = false
			}

			if own {
				// The kubeconfig is cluster admin credentials, so it must land
				// in the gitignored output directory, which on a fresh
				// checkout does not exist yet.
				if err := render.PrepareOutput(cfg, os.Stderr); err != nil {
					return err
				}
			}

			if err := tal.Stream(kubeconfigArgs(tc, target, dest, merge, force, extraFlags)...); err != nil {
				return err
			}

			if dest == "-" {
				return nil
			}

			fmt.Fprintf(os.Stderr, "wrote %s\n", render.Rel(dest))

			if own {
				fmt.Fprintf(os.Stderr, "  KUBECONFIG=%s kubectl get nodes\n", render.Rel(dest))
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to fetch from (default: the first one)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing context when merging")
	cmd.Flags().BoolVar(&merge, "merge", true,
		"merge into the destination instead of replacing it (default false for talman's own copy)")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// kubeconfigArgs builds the `talosctl kubeconfig` invocation. The destination
// is positional and goes last, after the escape hatch, so --extra-flags cannot
// displace it.
func kubeconfigArgs(talosconfig string, from *config.Node, dest string,
	merge, force bool, extraFlags []string,
) []string {
	args := []string{
		"--talosconfig", talosconfig,
		"kubeconfig",
		"--nodes", from.IPAddress,
		fmt.Sprintf("--merge=%t", merge),
	}

	if force {
		args = append(args, "--force")
	}

	args = append(args, extraFlags...)

	return append(args, dest)
}
