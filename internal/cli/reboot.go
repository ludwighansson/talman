package cli

import (
	"fmt"
	"os"
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
		yes        bool
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
		Long: `Reboot restarts nodes one at a time, in config order or rollout waves, waiting
for each to come back; control planes always alone, --parallel batches
workers. It lands a "talman apply --mode=staged". It names the nodes and
asks first, unless --yes. --health gates between nodes.

More in the README: "Rolling changes out safely", "Rolling out in waves".`,
		Args: cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if !slices.Contains(rebootModes, mode) {
				return fmt.Errorf("--mode %q is invalid: must be one of %s", mode, strings.Join(rebootModes, ", "))
			}

			return positive("timeout", timeout)
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

			// Refused before the question rather than after the answer: the
			// roll-out checks again, but a yes it then turns down is wasted.
			if !wait {
				if err := waitsForControlPlanes(targets); err != nil {
					return err
				}
			}

			if !yes && len(targets) > 0 {
				if err := confirmReboot(cfg.ClusterName, targets); err != nil {
					return err
				}
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

			ro := rollOut{
				cmd: cmd, cfg: cfg, tal: tal, tc: tc,
				verb: "reboot", done: "rebooted",
				targets: targets, parallel: parallel,
				health: health, timeout: timeout, waits: wait,
				stages: stages, soak: cfg.Rollout.SoakDuration(), thenFrom: thenFrom,
				// The nodes a resumed reboot names are asked about again.
				hintDrop: []string{"yes"},
			}

			if err := ro.run(rebootOne); err != nil {
				return err
			}

			rec.SetChanged(len(targets) > 0)
			ro.carryOn()

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes, by hostname or IP (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
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

// confirmReboot names the nodes about to be rebooted, in the order they go,
// and asks before any of them does.
func confirmReboot(clusterName string, targets []*config.Node) error {
	fmt.Fprintf(os.Stderr, "About to reboot %d node(s) in cluster %q:\n", len(targets), clusterName)

	for _, n := range targets {
		fmt.Fprintf(os.Stderr, "  %s (%s)\n", n.Hostname, n.IPAddress)
	}

	return askYes("reboot", "Continue?")
}
