package cli

import (
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// addAllowDowngrades is the one way past refuseDowngrades. what names the
// software a downgrade would move: "Talos", "Kubernetes".
func addAllowDowngrades(cmd *cobra.Command, allow *bool, what string) {
	cmd.Flags().BoolVar(allow, "allow-downgrades", false,
		"go ahead where a node runs a newer "+what+" than the target, which is otherwise refused")
}

// refuseDowngrades is the error for a run that would move nodes to an older
// version than they run, or nil when it would move none. doing names the run
// ("upgrading"), what the software, and fix how to aim the run higher instead.
//
// Refused rather than warned about: a talman.yaml checked out from before the
// last upgrade, or a version typed one digit short, reads exactly like a real
// change, and the roll-out that follows reboots or restarts every node on the
// way back down.
func refuseDowngrades(doing, what string, nodes []string, fix string) error {
	if len(nodes) == 0 {
		return nil
	}

	return fmt.Errorf("%s would downgrade %s on %d node(s): %s\n  %s, or pass --allow-downgrades "+
		"if a downgrade is what you mean", doing, what, len(nodes), strings.Join(nodes, ", "), fix)
}

// talosDowngrades names the targets that run a newer Talos than they are
// configured for, each as "host running -> want".
//
// Every target is asked before any is upgraded: found when the roll-out
// reached it, the run would stop half way through a cluster. A node that does
// not answer is not counted -- talman cannot see a downgrade there, and the
// upgrade reports the node itself when it gets to it.
func talosDowngrades(cfg *config.Config, tal *talosctl.Runner, tc string, targets []*config.Node) []string {
	running, _ := eachNode(targets, defaultParallel, func(n *config.Node) (string, error) {
		return tal.TalosVersion(tc, n.IPAddress)
	})

	var out []string

	for i, n := range targets {
		if want := n.EffectiveTalosVersion(cfg); newer(running[i], want) {
			out = append(out, fmt.Sprintf("%s %s -> %s", n.Hostname, running[i], want))
		}
	}

	return out
}

// k8sDowngrades names the nodes whose kubelet runs a newer Kubernetes than
// want, each as "host running -> want", asked and counted as talosDowngrades
// does. The kubelet answers for the node as it does for upgrade-k8s: it is the
// one Kubernetes component every node runs.
func k8sDowngrades(tal *talosctl.Runner, tc string, nodes []*config.Node, want string) []string {
	running, _ := eachNode(nodes, defaultParallel, func(n *config.Node) (string, error) {
		return tal.KubeletVersion(tc, n.IPAddress)
	})

	var out []string

	for i, n := range nodes {
		if newer(running[i], want) {
			out = append(out, fmt.Sprintf("%s %s -> %s", n.Hostname, withV(running[i]), withV(want)))
		}
	}

	return out
}

// newer reports whether running is a later version than want, which makes
// moving a node from one to the other a downgrade. A version that was not
// read, or does not parse, is no answer, and is not taken for one.
func newer(running, want string) bool {
	r, err := semver.NewVersion(running)
	if err != nil {
		return false
	}

	w, err := semver.NewVersion(want)
	if err != nil {
		return false
	}

	return r.GreaterThan(w)
}
