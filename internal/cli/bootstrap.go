package cli

import (
	"fmt"
	"os"

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
plane node, after that node's config has been applied.

Talos accepts the request and returns: etcd starts afterwards and the control
plane forms over the following minute or so, so a successful bootstrap is the
beginning of the cluster rather than the end of the command.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			rec := currentRun

			_, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			rec.NodeStart(target.Hostname, string(target.Role))

			fmt.Fprintf(os.Stderr, "== bootstrapping etcd on %s (%s)\n", target.Hostname, target.IPAddress)

			args := make([]string, 0, 5+len(extraFlags))
			args = append(args, "--talosconfig", tc, "bootstrap", "--nodes", target.IPAddress)

			if err := tal.Stream(append(args, extraFlags...)...); err != nil {
				rec.NodeDone(target.Hostname, err)

				return err
			}

			rec.NodeChanged(target.Hostname, true)
			rec.NodeDone(target.Hostname, nil)

			// talosctl prints nothing on success, which left the one command
			// in a cluster's life that can only be run once looking like it
			// had done nothing at all.
			fmt.Fprintf(os.Stderr, "cluster bootstrap initiated; etcd is starting on %s\n"+
				"  talman status      to watch the control plane come up\n"+
				"  talman kubeconfig  once it is serving\n", target.Hostname)

			return nil
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to bootstrap (default: the first one)")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}
