package cli

import (
	"fmt"

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

// controlPlane resolves a named control plane node, or the first one in the
// config when nothing is named.
func controlPlane(cfg *config.Config, name string) (*config.Node, error) {
	if name == "" {
		cps := cfg.ControlPlanes()
		if len(cps) == 0 {
			return nil, fmt.Errorf("no control plane nodes in the config")
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
