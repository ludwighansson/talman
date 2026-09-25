package cli

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// controlPlaneTarget resolves the control plane node a cluster-wide command
// should talk to.
func controlPlaneTarget(name string) (*config.Config, *talosctl.Runner, string, *config.Node, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, "", nil, err
	}

	target, err := controlPlane(cfg, name)
	if err != nil {
		return nil, nil, "", nil, err
	}

	tc, err := ensureTalosconfig(cfg)
	if err != nil {
		return nil, nil, "", nil, err
	}

	return cfg, runner(cfg), tc, target, nil
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

// controlPlane resolves a named control plane node, or the first one in the
// config when nothing is named.
func controlPlane(cfg *config.Config, name string) (*config.Node, error) {
	if name == "" {
		cps := cfg.ControlPlanes()
		if len(cps) == 0 {
			return nil, errors.New("no control plane nodes in the config")
		}

		return cps[0], nil
	}

	n, ok := cfg.Node(name)
	if !ok {
		return nil, fmt.Errorf("no node %q in the config", name)
	}

	if !n.IsControlPlane() {
		return nil, fmt.Errorf("node %s is a worker; this command needs a control plane node", name)
	}

	return n, nil
}

// clusterCheck answers "is there no cluster yet?", asking ask until it knows.
//
// A known answer is kept for the run; "unknown" is not, and is asked again
// next time. The first ask in a fresh build comes moments after the first
// control plane took its config, before its etcd service exists, and keeping
// that "unknown" made every later node wait for a cluster that cannot exist
// until bootstrap -- a ten-minute timeout at the first worker.
//
// Deliberately not "the probe said no": unknown is not absent. A probe that
// could not reach a control plane has established nothing, and what this
// gates -- waiting for a node, and the health check between nodes -- is what
// keeps one bad config from reaching a whole control plane. Silence must not
// switch those off.
func clusterCheck(ask func() clusterState) func() bool {
	var (
		mu    sync.Mutex
		known *clusterState
	)

	return func() bool {
		mu.Lock()
		defer mu.Unlock()

		if known == nil {
			if state := ask(); state != clusterUnknown {
				known = &state
			} else {
				return false
			}
		}

		return *known == clusterAbsent
	}
}
