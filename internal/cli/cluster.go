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

	var target *config.Node

	if name != "" {
		n, ok := cfg.Node(name)
		if !ok {
			return nil, nil, "", nil, fmt.Errorf("no node %q in the config", name)
		}

		if !n.IsControlPlane() {
			return nil, nil, "", nil, fmt.Errorf("node %s is a worker; this command needs a control plane node", name)
		}

		target = n
	} else {
		cps := cfg.ControlPlanes()
		if len(cps) == 0 {
			return nil, nil, "", nil, fmt.Errorf("no control plane nodes in the config")
		}

		target = cps[0]
	}

	tc, err := requireTalosconfig(cfg)
	if err != nil {
		return nil, nil, "", nil, err
	}

	return cfg, runner(cfg), tc, target, nil
}
