package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// bootstrapFlagsAllowed refuses what `apply --bootstrap` cannot be combined
// with. It builds the whole cluster, first control plane first, so a
// selection of nodes or waves would leave out the node the cluster is built
// on. It has to see the first control plane come back before bootstrapping
// etcd on it, so it cannot skip the wait, and it has to ask each node which
// API it answers, so it cannot be told one. A --dry-run is its plan.
func bootstrapFlagsAllowed(cmd *cobra.Command, mode string, onlyNew, wait bool) error {
	for _, name := range []string{"node", "group", "wave", "from", "until", "insecure"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--bootstrap builds the whole cluster, asking each node what it is, "+
				"so it takes no --%s", name)
		}
	}

	switch {
	case !wait:
		return errors.New("--bootstrap waits for the first control plane before bootstrapping etcd on it, " +
			"so it takes no --wait=false")
	case mode != "auto":
		return fmt.Errorf("--bootstrap applies configs as they are, so it takes no --mode=%s", mode)
	case onlyNew:
		return errors.New("--bootstrap onboards every node already; --only-new-nodes has nothing to add")
	}

	return nil
}

// cpState is what a control plane answered: which API, and, if it answers
// with cluster PKI, whether it runs etcd.
type cpState struct {
	node *config.Node
	mode talosctl.Mode
	etcd talosctl.EtcdState
}

// probeControlPlanes asks every control plane what it is, all at once: a
// control plane that is down costs dial timeouts, and asking them one after
// another would make every apply pay them all in turn.
func probeControlPlanes(cfg *config.Config, tal *talosctl.Runner, tc string) []cpState {
	states, _ := eachNode(cfg.ControlPlanes(), defaultParallel, func(n *config.Node) (cpState, error) {
		st := cpState{node: n, mode: tal.Mode(tc, n.IPAddress)}

		if st.mode == talosctl.ModeRunning {
			st.etcd = tal.Etcd(tc, n.IPAddress)
		}

		return st, nil
	})

	return states
}

// allNew reports whether every control plane is new -- in maintenance mode,
// never configured -- which is the one state talman can be sure means no
// cluster was ever bootstrapped.
func allNew(states []cpState) bool {
	for _, st := range states {
		if st.mode != talosctl.ModeMaintenance {
			return false
		}
	}

	return len(states) > 0
}

// noEtcd reports whether control planes answered with cluster PKI and none of
// them runs etcd: a cluster never bootstrapped, or one whose etcd is broken.
// From outside, the two look the same.
func noEtcd(states []cpState) bool {
	answered := false

	for _, st := range states {
		if st.mode != talosctl.ModeRunning {
			continue
		}

		if st.etcd != talosctl.EtcdStopped {
			return false
		}

		answered = true
	}

	return answered
}

// checkBootstrappable checks there is nothing bootstrapped for --bootstrap to
// split, and returns the control planes that are configured already -- which
// may be a cluster an earlier run half built, or one whose etcd is broken.
//
// Every control plane has to answer, and none may run etcd: bootstrapping a
// second time splits a cluster, and a control plane talman cannot ask might
// be the one running it.
func checkBootstrappable(cfg *config.Config, states []cpState) ([]*config.Node, error) {
	if len(states) == 0 {
		return nil, errors.New("--bootstrap needs a control plane in the config")
	}

	var configured []*config.Node

	for _, st := range states {
		n := st.node

		switch {
		case st.mode == talosctl.ModeUnreachable:
			return nil, fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service, so "+
				"talman cannot tell whether it runs etcd, and will not risk bootstrapping a second cluster",
				n.Hostname, n.IPAddress)
		case st.mode == talosctl.ModeMaintenance:
			continue
		case st.etcd == talosctl.EtcdRunning:
			return nil, fmt.Errorf("%s (%s) already runs etcd: the cluster is bootstrapped, and doing it "+
				"again would split it\n  %s", n.Hostname, n.IPAddress, talmanCmd("apply"))
		case st.etcd == talosctl.EtcdUnknown:
			return nil, fmt.Errorf("%s (%s) answers, but talman cannot tell whether it runs etcd, and will "+
				"not risk bootstrapping a second cluster", n.Hostname, n.IPAddress)
		}

		configured = append(configured, n)
	}

	return configured, nil
}

// confirmConfigured asks before bootstrapping control planes that are
// configured but run no etcd. That is a cluster an earlier run half built --
// or one whose etcd is broken, and a bootstrap sent to one of those stops
// its etcd before Talos refuses it. talman cannot tell the two apart, so the
// operator says which.
func confirmConfigured(cfg *config.Config, configured []*config.Node, yes bool) error {
	if len(configured) == 0 || yes {
		return nil
	}

	names := make([]string, 0, len(configured))
	for _, n := range configured {
		names = append(names, n.Hostname)
	}

	return typeClusterName("bootstrap", cfg.ClusterName, fmt.Sprintf(
		"%s %s configured but %s no etcd: either this cluster was never bootstrapped, or its etcd is "+
			"broken, and bootstrapping a broken cluster stops its etcd.",
		strings.Join(names, ", "), plural(len(names), "is", "are"), plural(len(names), "runs", "run")))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}

	return many
}

// talmanCmd spells out a talman command for a hint, with the -c the run was
// given, so the command reaches the same cluster.
func talmanCmd(rest string) string {
	return "talman" + configArg() + " " + rest
}

// printBootstrapPlan is --bootstrap --dry-run: the order the build would
// take, and which nodes are new, without sending anything -- a dry run to a
// node in maintenance mode would ship it the config, unauthenticated.
func printBootstrapPlan(cfg *config.Config, tal *talosctl.Runner, tc string, first *config.Node,
	stages []config.Staged,
) {
	status := func(n *config.Node) string {
		switch tal.Mode(tc, n.IPAddress) {
		case talosctl.ModeMaintenance:
			return n.Hostname + " (new)"
		case talosctl.ModeRunning:
			return n.Hostname + " (configured)"
		default:
			return n.Hostname + " (not answering)"
		}
	}

	fmt.Fprintf(os.Stderr, "would configure %s, bootstrap etcd on it, then:\n", status(first))

	for i, st := range stages {
		names := make([]string, 0, len(st.Nodes))
		for _, n := range st.Nodes {
			names = append(names, status(n))
		}

		fmt.Fprintf(os.Stderr, "  %d/%d %s: %s\n", i+1, len(stages), st.Name(), strings.Join(names, ", "))
	}

	fmt.Fprintln(os.Stderr, "nothing was sent")
}

// bootstrapFirst configures the first control plane, then bootstraps etcd on
// it and waits until etcd runs, so that every node after it joins a cluster
// that exists. apply is the node's own apply, which waits for it to come back
// as any apply does.
func bootstrapFirst(cmd *cobra.Command, tal *talosctl.Runner, tc string, first *config.Node,
	timeout time.Duration, apply func(say func(string)) error,
) error {
	rec := currentRun
	say := func(s string) { fmt.Fprint(os.Stderr, s) }

	// The whole run again, not a list of nodes: until etcd runs there is no
	// cluster, and the run that builds one is this one.
	again := "  continue with: " + talmanCmd("apply --bootstrap"+resumeFlags(cmd, "bootstrap"))

	rec.NodeStart(first.Hostname, string(first.Role))

	err := apply(say)
	rec.NodeDone(first.Hostname, err)

	if err != nil {
		return fmt.Errorf("%w\n%s", err, again)
	}

	fmt.Fprintf(os.Stderr, "== bootstrapping etcd on %s (%s)\n", first.Hostname, first.IPAddress)

	if err := tal.Stream(append(tal.NodeArgs(tc, first.IPAddress), "bootstrap")...); err != nil {
		return fmt.Errorf("%w\n%s", err, again)
	}

	if err := waitForEtcd(tal, tc, first, timeout); err != nil {
		return fmt.Errorf("%w\n  etcd was bootstrapped on %s: do not bootstrap again. Once it runs "+
			"(talman status shows it), carry on with:\n    %s", err, first.Hostname,
			talmanCmd("apply --onboard-new-nodes"))
	}

	return nil
}

// waitForEtcd waits until etcd runs on n, saying so while it waits: Talos
// accepts a bootstrap and returns, and etcd starts in the minute after.
func waitForEtcd(tal *talosctl.Runner, tc string, n *config.Node, timeout time.Duration) error {
	started := time.Now()
	deadline := started.Add(timeout)
	lastReport := time.Time{}

	for {
		if tal.Etcd(tc, n.IPAddress) == talosctl.EtcdRunning {
			fmt.Fprintf(os.Stderr, "%setcd is running on %s after %s\n", detail, n.Hostname,
				time.Since(started).Round(time.Second))

			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("etcd did not start on %s within %s (--timeout waits longer)", n.Hostname, timeout)
		}

		if time.Since(lastReport) >= 30*time.Second {
			lastReport = time.Now()

			fmt.Fprintf(os.Stderr, "%swaiting for etcd to start on %s (%s elapsed)\n", detail, n.Hostname,
				time.Since(started).Round(time.Second))
		}

		if err := interrupt.Sleep(5 * time.Second); err != nil {
			return err
		}
	}
}
