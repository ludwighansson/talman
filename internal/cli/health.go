package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newHealthCmd() *cobra.Command {
	var serverSide bool

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check cluster health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			tc, err := requireTalosconfig(cfg)
			if err != nil {
				return err
			}

			var cps, workers []string

			for i := range cfg.Nodes {
				n := &cfg.Nodes[i]
				if n.IsControlPlane() {
					cps = append(cps, n.IPAddress)
				} else {
					workers = append(workers, n.IPAddress)
				}
			}

			args := []string{"--talosconfig", tc, "health", fmt.Sprintf("--server=%t", serverSide)}

			if len(cps) > 0 {
				args = append(args, "--control-plane-nodes", strings.Join(cps, ","))
			}

			if len(workers) > 0 {
				args = append(args, "--worker-nodes", strings.Join(workers, ","))
			}

			return runner(cfg).Stream(args...)
		},
	}

	cmd.Flags().BoolVar(&serverSide, "server", true, "run the health check on the node rather than the client")

	return cmd
}
