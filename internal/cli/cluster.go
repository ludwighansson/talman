package cli

import (
	"fmt"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// controlPlaneTarget resolves the control plane node a cluster-wide command
// should talk to: the one named, or else the first in the config that
// answers.
//
// The first that answers rather than the first listed: a command that acts on
// the cluster as a whole, driven through the one control plane that happens
// to be down, fails on that machine while the rest of the cluster serves.
func controlPlaneTarget(name string) (*config.Config, *talosctl.Runner, string, *config.Node, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, "", nil, err
	}

	var target *config.Node

	// Named, it is checked before anything is generated: a mistyped name
	// should not cost decrypting the bundle.
	if name != "" {
		if target, err = controlPlane(cfg, name); err != nil {
			return nil, nil, "", nil, err
		}
	}

	tc, err := ensureTalosconfig(cfg)
	if err != nil {
		return nil, nil, "", nil, err
	}

	tal := runner(cfg)

	if target == nil {
		if target, err = healthNode(cfg, tal, tc); err != nil {
			return nil, nil, "", nil, err
		}
	}

	return cfg, tal, tc, target, nil
}

// clusterState is what talman could establish about whether a cluster exists.
type clusterState int

const (
	// clusterUnknown is no control plane answering. It is not evidence that
	// there is no cluster, and nothing may be concluded from it.
	clusterUnknown clusterState = iota
	// clusterAbsent is control planes answering with no etcd running: nothing
	// has been bootstrapped.
	clusterAbsent
	// clusterUp is etcd running somewhere, healthy or not.
	clusterUp
)

// askCluster asks the configured control planes whether there is a cluster to
// join.
//
// Every control plane in the config, not merely the ones a command happened to
// select: "no control plane answered" is a different fact from "no control
// plane has etcd", and only the second one means anything. Telling an operator
// to bootstrap because the machine they asked about is a worker, or because
// the control planes are briefly unreachable, is advice that destroys a
// cluster.
func askCluster(cfg *config.Config, tal *talosctl.Runner, talosconfig string) clusterState {
	state := clusterUnknown

	for _, cp := range cfg.ControlPlanes() {
		switch tal.Etcd(talosconfig, cp.IPAddress) {
		case talosctl.EtcdRunning:
			return clusterUp
		case talosctl.EtcdStopped:
			state = clusterAbsent
		case talosctl.EtcdUnknown:
		}
	}

	return state
}

// controlPlane resolves a named control plane node.
func controlPlane(cfg *config.Config, name string) (*config.Node, error) {
	n, ok := cfg.Node(name)
	if !ok {
		return nil, fmt.Errorf("no node %q in the config", name)
	}

	if !n.IsControlPlane() {
		return nil, fmt.Errorf("node %s is a worker; this command needs a control plane node", name)
	}

	return n, nil
}
