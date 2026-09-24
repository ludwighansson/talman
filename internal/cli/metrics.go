package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/metrics"
)

// metricsOpts is where a run's metrics go, from the global flags or, for a CI
// job that would rather set them once, the environment.
type metricsOpts struct {
	file   string
	url    string
	labels []string
}

var metricsFlags metricsOpts

// currentRun is the run the executing command is recording, nil when metrics
// are off. A command runs once per process, so one is all there is.
var currentRun *metrics.Run

func addMetricsFlags(cmd *cobra.Command) {
	cmd.PersistentFlags().StringVar(&metricsFlags.file, "metrics-file", "",
		"write the run's metrics to this file, in the Prometheus text format ($TALMAN_METRICS_FILE)")
	cmd.PersistentFlags().StringVar(&metricsFlags.url, "metrics-url", "",
		"push the run's metrics to this metrics push endpoint ($TALMAN_METRICS_URL)")
	cmd.PersistentFlags().StringArrayVar(&metricsFlags.labels, "metrics-label", nil,
		"add a key=value label to every metric (repeatable; $TALMAN_METRICS_LABELS, comma-separated)")
}

// recorded makes a command record its run as metrics, when metrics are on.
//
// Recorded however the command ends: a failed run is the one an alert exists
// for. Neither a file that cannot be written nor an endpoint that cannot be
// reached changes the command's exit code -- the run did what it did, and a
// deployment that succeeded is not made to fail by its monitoring.
func recorded(cmd *cobra.Command) *cobra.Command {
	inner := cmd.RunE

	cmd.RunE = func(c *cobra.Command, args []string) error {
		run, err := startRun(c.Name())
		if err != nil {
			return err
		}

		currentRun = run
		defer func() { currentRun = nil }()

		err = inner(c, args)

		run.Finish(err == nil || errors.Is(err, errChanged))
		report(run)

		return err
	}

	return cmd
}

// startRun begins recording, or returns nil when there is nowhere to send the
// result.
func startRun(command string) (*metrics.Run, error) {
	file := firstNonEmpty(metricsFlags.file, os.Getenv("TALMAN_METRICS_FILE"))
	url := firstNonEmpty(metricsFlags.url, os.Getenv("TALMAN_METRICS_URL"))

	if file == "" && url == "" {
		return nil, nil //nolint:nilnil // no run is the answer when metrics are off
	}

	raw := metricsFlags.labels

	// Flags win over the environment, label by label, so a job can set its
	// common labels once and one step can override one of them.
	if env := os.Getenv("TALMAN_METRICS_LABELS"); env != "" {
		raw = append(strings.Split(env, ","), raw...)
	}

	labels := map[string]string{}

	for _, l := range raw {
		if strings.TrimSpace(l) == "" {
			continue
		}

		key, value, err := metrics.ParseLabel(l)
		if err != nil {
			return nil, err
		}

		labels[key] = value
	}

	return metrics.NewRun(command, Version, labels), nil
}

func report(run *metrics.Run) {
	if run == nil {
		return
	}

	if file := firstNonEmpty(metricsFlags.file, os.Getenv("TALMAN_METRICS_FILE")); file != "" {
		if err := run.WriteFile(file); err != nil {
			fmt.Fprintf(os.Stderr, "warning: metrics were not written: %v\n", err)
		}
	}

	if url := firstNonEmpty(metricsFlags.url, os.Getenv("TALMAN_METRICS_URL")); url != "" {
		if err := run.Push(context.Background(), url, os.Getenv("TALMAN_METRICS_TOKEN")); err != nil {
			fmt.Fprintf(os.Stderr, "warning: metrics were not pushed: %v\n", err)
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
