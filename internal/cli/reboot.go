package cli

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
)

// rebootModes are the modes `talosctl reboot --mode` accepts.
var rebootModes = []string{"default", "powercycle", "force"}

func newRebootCmd() *cobra.Command {
	var (
		nodes      []string
		mode       string
		wait       bool
		health     bool
		timeout    time.Duration
		parallel   int
		waves      waveFlags
		extraFlags []string
	)

	cmd := &cobra.Command{
		Use:   "reboot",
		Short: "Reboot nodes one at a time, waiting for each to come back",
		Long: `Reboot is a rolling reboot: nodes go one at a time in config order, and talosctl
waits for each to come back before the next begins. --parallel raises that for
workers only; a control plane always reboots alone, because two rebooting
together is how a three-node control plane loses quorum.

It is the second half of "talman apply --mode=staged", whose config lands on
the next reboot, and the way to pick up anything else that only a reboot
applies.

--health adds a cluster health check between nodes, and stops the roll-out if
the cluster is unhealthy. As with apply and upgrade, it is off by default.`,
		Args: cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if !slices.Contains(rebootModes, mode) {
				return fmt.Errorf("--mode %q is invalid: must be one of %s", mode, strings.Join(rebootModes, ", "))
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec := currentRun

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := selectNodes(cfg, nodes)
			if err != nil {
				return err
			}

			stages, targets, thenFrom, err := waves.plan(cfg, targets)
			if err != nil {
				return err
			}

			for _, n := range targets {
				rec.Plan(n.Hostname, string(n.Role))
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			tal := runner(cfg)

			rebootOne := func(n *config.Node, grouped bool, say func(string)) (bool, error) {
				args := append([]string{
					"--talosconfig", tc,
					"reboot",
					"--nodes", n.IPAddress,
					"--mode", mode,
					fmt.Sprintf("--wait=%t", wait),
					"--timeout", timeout.String(),
				}, extraFlags...)

				header := fmt.Sprintf("== rebooting %s (%s)\n", n.Hostname, n.IPAddress)

				if err := runTalosctl(tal, grouped, header, say, args); err != nil {
					return false, err
				}

				rec.NodeChanged(n.Hostname, true)

				return true, nil
			}

			if err := (rollOut{
				cmd: cmd, cfg: cfg, tal: tal, tc: tc,
				verb: "reboot", done: "rebooted",
				targets: targets, parallel: parallel,
				health: health, timeout: timeout, waits: wait,
				stages: stages, soak: cfg.Rollout.SoakDuration(), thenFrom: thenFrom,
			}).run(rebootOne); err != nil {
				return err
			}

			rec.SetChanged(len(targets) > 0)

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().StringVarP(&mode, "mode", "m", "default",
		"reboot mode: default, powercycle (bypass kexec) or force (skip graceful shutdown)")
	cmd.Flags().BoolVar(&wait, "wait", true, "wait for each node to come back before moving on")
	cmd.Flags().BoolVar(&health, "health", false,
		"run a cluster health check between nodes, and stop if it fails")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute,
		"how long to wait for each node to come back, and for the health check between nodes")
	addParallelFlag(cmd, &parallel, 1,
		"how many workers to reboot at once; control planes always go one at a time")
	addWaveFlags(cmd, &waves)
	addExtraFlags(cmd, &extraFlags)

	return cmd
}
