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
		direct     bool
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Wipe nodes and return them to maintenance mode",
		Long: `Reset wipes a node's disks and returns it to maintenance mode. This destroys
all data on the node and, if run against enough control planes, the cluster.

Workers are reset before control planes, and each node is reached at its own
address rather than through the talosconfig endpoints. Both exist for the same
reason: the endpoints are the control planes, so wiping those first destroys
the path to every node still waiting -- and a graceful reset needs a live
cluster to leave.`,
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

			// Before the confirmation, so the order shown is the order run.
			targets = resetOrder(targets)

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

			for i, n := range targets {
				args := []string{"--talosconfig", tc}

				// Ahead of the subcommand: --endpoints is one of talosctl's
				// global flags, and it overrides the talosconfig's list for
				// this call only.
				if direct {
					args = append(args, "--endpoints", n.IPAddress)
				}

				args = append(args,
					"reset",
					"--nodes", n.IPAddress,
					fmt.Sprintf("--graceful=%t", graceful),
				)

				args = append(args, extraFlags...)

				fmt.Fprintf(os.Stderr, "== resetting %s (%s)\n", n.Hostname, n.IPAddress)

				if err := tal.Stream(args...); err != nil {
					return fmt.Errorf("%w\n%s", err, unreset(targets[i:], graceful))
				}
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt")
	cmd.Flags().BoolVar(&graceful, "graceful", true, "leave etcd cleanly before resetting")
	cmd.Flags().BoolVar(&direct, "direct", true,
		"reach each node at its own address instead of proxying through the talosconfig endpoints")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// resetOrder puts workers before control planes.
//
// talman reaches a node through the talosconfig endpoints, and those are the
// control planes. Config order lists control planes first, so a whole-cluster
// reset used to cut its own path partway through: three control planes wiped,
// then the first worker's reset proxied through one of them and timed out.
//
// It is the right order for the cluster too. A graceful reset asks the node to
// leave etcd and the Kubernetes API, which needs a control plane still
// serving; taking the control planes out first turns every remaining graceful
// reset into a failure.
//
// Stable within each group, so nodes still go in config order.
func resetOrder(targets []*config.Node) []*config.Node {
	out := make([]*config.Node, 0, len(targets))

	for _, n := range targets {
		if !n.IsControlPlane() {
			out = append(out, n)
		}
	}

	for _, n := range targets {
		if n.IsControlPlane() {
			out = append(out, n)
		}
	}

	return out
}

// unreset names what a failed pass left behind.
//
// A reset stops at the first failure, which leaves the operator holding a
// half-wiped cluster and a scroll-back to read the remainder out of. The
// failed node is part of it: its reset did not complete either.
func unreset(remaining []*config.Node, graceful bool) string {
	names := make([]string, 0, len(remaining))
	resume := "talman reset"

	for _, n := range remaining {
		names = append(names, n.Hostname)
		resume += " -n " + n.Hostname
	}

	if !graceful {
		resume += " --graceful=false"
	}

	return fmt.Sprintf("  %d node(s) were not reset: %s\n  continue with: %s",
		len(names), strings.Join(names, ", "), resume)
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
