package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
)

func newResetCmd() *cobra.Command {
	var (
		nodes      []string
		yes        bool
		graceful   bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe nodes and return them to maintenance mode",
		Long: `Reset wipes a node's disks and returns it to maintenance mode. This destroys
all data on the node and, if run against enough control planes, the cluster.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			if !yes {
				if err := confirm(cfg.ClusterName, targets); err != nil {
					return err
				}
			}

			tal := runner(cfg)

			for _, n := range targets {
				args := []string{
					"--talosconfig", tc,
					"reset",
					"--nodes", n.IPAddress,
					fmt.Sprintf("--graceful=%t", graceful),
				}

				args = append(args, extraFlags...)

				fmt.Fprintf(os.Stderr, "== resetting %s (%s)\n", n.Hostname, n.IPAddress)

				if err := tal.Stream(args...); err != nil {
					return err
				}
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&graceful, "graceful", true, "leave etcd cleanly before resetting")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// confirm requires the operator to type the cluster name, so a reset cannot
// happen because a script passed the wrong config file.
func confirm(clusterName string, targets []*config.Node) error {
	fmt.Fprintf(os.Stderr, "About to reset %d node(s) in cluster %q:\n", len(targets), clusterName)

	for _, n := range targets {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n", n.Hostname, n.IPAddress)
	}

	fmt.Fprintf(os.Stderr, "This destroys all data on them. Type the cluster name to continue: ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reset aborted: %w", err)
	}

	if strings.TrimSpace(line) != clusterName {
		return fmt.Errorf("reset aborted: input did not match %q", clusterName)
	}

	return nil
}
