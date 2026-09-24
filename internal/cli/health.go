package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newHealthCmd() *cobra.Command {
	var (
		node       string
		serverSide bool
		timeout    time.Duration
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "health",
		Short: "Check cluster health",
		Long: `Health runs "talosctl health", which checks etcd, the control plane static
pods and every node's readiness.

The check runs from one control plane node -- the first in the config unless
--node names another -- and reports on the whole cluster, not on that node.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, tal, tc, target, err := controlPlaneTarget(node)
			if err != nil {
				return err
			}

			// With no node named, prefer one that is answering: a check
			// driven from a control plane that is down reports the cluster as
			// broken when what is broken is the entry point.
			if node == "" {
				target, err = healthNode(cfg, tal, tc)
				if err != nil {
					return err
				}
			}

			// Unbounded unless asked: `talman health` was run on purpose, and
			// talosctl's own patience is the right default for a command
			// whose whole job is to wait for a cluster.
			args := append(healthArgs(cfg, tc, target, serverSide, timeout), extraFlags...)

			return tal.Stream(args...)
		},
	}

	cmd.Flags().StringVarP(&node, "node", "n", "", "control plane node to run the check from (default: the first one)")
	cmd.Flags().BoolVar(&serverSide, "server", true, "run the health check on the node rather than the client")
	cmd.Flags().DurationVar(&timeout, "timeout", 0,
		"how long to wait for the cluster to become healthy (0: talosctl's own default)")
	addExtraFlags(cmd, &extraFlags)

	return cmd
}

// healthNode picks the control plane a health check runs from: the first one
// in the config that answers the Talos API.
//
// Answering matters because the alternative is reporting a whole cluster as
// broken when the single node talman chose to ask happens to be the one that
// is down. `talosctl version` is the cheapest call that proves apid is
// serving, and the probe stops at the first success.
//
// Not the VIP, tempting as it is. A Talos VIP is only held by a control plane
// that is in etcd quorum, so it vanishes during exactly the outage worth
// checking, and as a --nodes value it names "whichever node holds it right
// now" -- ambiguous in a report that is per node.
//
// Failover lives here rather than in the endpoint list: the check is pinned to
// the node this picks, so walking the control planes until one answers is what
// survives a dead one. Leaving the list to talosctl would let it dial a
// control plane that is down and report a connection failure as a cluster
// verdict.
func healthNode(cfg *config.Config, tal *talosctl.Runner, talosconfig string) (*config.Node, error) {
	cps := cfg.ControlPlanes()
	if len(cps) == 0 {
		return nil, errors.New("no control plane nodes in the config: nothing can answer for cluster health")
	}

	addrs := make([]string, 0, len(cps))

	for _, n := range cps {
		if tal.Reachable(talosconfig, n.IPAddress) {
			return n, nil
		}

		addrs = append(addrs, n.IPAddress)
	}

	return nil, fmt.Errorf("no control plane node answered the Talos API (tried %s): "+
		"the cluster cannot be checked from here", strings.Join(addrs, ", "))
}

// healthArgs builds a `talosctl health` invocation that reports on the whole
// cluster from one node.
//
// --nodes matters as much as the two node lists. health accepts exactly one
// node to run from, and with none given talosctl falls back to the
// talosconfig's default node list -- which talman sets to every node in the
// cluster, so the command died with `requires exactly one node (got 6)` before
// checking anything.
//
// The node lists come from the config rather than from the cluster, so a node
// that is supposed to exist and does not is a failure rather than an absence.
func healthArgs(cfg *config.Config, talosconfig string, from *config.Node, serverSide bool,
	waitFor time.Duration,
) []string {
	args := []string{
		"--talosconfig", talosconfig,
		// Pinned to the node the check runs from, for the same reason the
		// probes are: the talosconfig's endpoints include control planes that
		// may not be serving, and dialling one of those to ask about the
		// cluster fails on the connection rather than on the cluster.
		"--endpoints", from.IPAddress,
		"health",
		"--nodes", from.IPAddress,
		fmt.Sprintf("--server=%t", serverSide),
	}

	// talosctl waits 20 minutes by default, which is a long time to find out
	// that a gate is not going to pass. A caller that has its own patience --
	// apply, which already knows how long it will wait for a node -- says so.
	if waitFor > 0 {
		args = append(args, "--wait-timeout", waitFor.String())
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

	if len(cps) > 0 {
		args = append(args, "--control-plane-nodes", strings.Join(cps, ","))
	}

	if len(workers) > 0 {
		args = append(args, "--worker-nodes", strings.Join(workers, ","))
	}

	return args
}
