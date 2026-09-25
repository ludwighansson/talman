package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// bootstrapFlagsAllowed refuses what `apply --bootstrap` cannot be combined
// with. It builds the whole cluster, first control plane first, so a
// selection of nodes or waves would leave out the node the cluster is built
// on; and it enacts, so a dry run or a staged apply would bootstrap etcd on a
// node that has not taken its config.
func bootstrapFlagsAllowed(cmd *cobra.Command, dryRun bool, mode string, onlyNew bool) error {
	for _, name := range []string{"node", "group", "wave", "from", "until"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--bootstrap builds the whole cluster, so it takes no --%s", name)
		}
	}

	switch {
	case dryRun:
		return errors.New("--bootstrap cannot be a dry run: etcd is bootstrapped on a node that took its config")
	case mode != "auto":
		return fmt.Errorf("--bootstrap applies configs as they are, so it takes no --mode=%s", mode)
	case onlyNew:
		return errors.New("--bootstrap onboards every node already; --only-new-nodes has nothing to add")
	}

	return nil
}

// notBootstrapped checks there is no cluster yet for --bootstrap to build.
//
// Every control plane has to answer -- in maintenance mode, or configured and
// waiting for etcd -- and none may run etcd: bootstrapping a second time
// splits the cluster in two, which is the one thing this command must never
// do. A control plane talman cannot ask is a refusal too, since it might be
// the one running etcd.
func notBootstrapped(cfg *config.Config, tal *talosctl.Runner, tc string) error {
	cps := cfg.ControlPlanes()
	if len(cps) == 0 {
		return errors.New("--bootstrap needs a control plane in the config")
	}

	for _, n := range cps {
		switch tal.Mode(tc, n.IPAddress) {
		case talosctl.ModeMaintenance:
			continue
		case talosctl.ModeUnreachable:
			return fmt.Errorf("%s (%s) answers neither the Talos API nor the maintenance service, so talman "+
				"cannot tell whether it runs etcd, and will not risk bootstrapping a second cluster",
				n.Hostname, n.IPAddress)
		case talosctl.ModeRunning:
		}

		switch tal.Etcd(tc, n.IPAddress) {
		case talosctl.EtcdRunning:
			return fmt.Errorf("%s (%s) already runs etcd: the cluster is bootstrapped, and doing it "+
				"again would split it\n  talman apply", n.Hostname, n.IPAddress)
		case talosctl.EtcdUnknown:
			return fmt.Errorf("%s (%s) answers, but talman cannot tell whether it runs etcd, and will not "+
				"risk bootstrapping a second cluster", n.Hostname, n.IPAddress)
		case talosctl.EtcdStopped:
		}
	}

	return nil
}

// unbootstrapped reports whether the cluster is plainly not bootstrapped: a
// control plane answered that it runs no etcd and none that it does, or every
// control plane is still in maintenance mode.
//
// Only a definite answer counts. Control planes that cannot be reached have
// established nothing, and an apply refused because the cluster was briefly
// unreachable would be the wrong advice for a cluster in trouble.
func unbootstrapped(cfg *config.Config, tal *talosctl.Runner, tc string) bool {
	switch askCluster(cfg, tal, tc) {
	case clusterAbsent:
		return true
	case clusterUp:
		return false
	case clusterUnknown:
	}

	cps := cfg.ControlPlanes()

	for _, n := range cps {
		if tal.Mode(tc, n.IPAddress) != talosctl.ModeMaintenance {
			return false
		}
	}

	return len(cps) > 0
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
	again := "  continue with: talman apply --bootstrap" + resumeFlags(cmd, "bootstrap")

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
		return fmt.Errorf("%w\n  etcd was bootstrapped on %s: do not bootstrap again; once it runs, "+
			"carry on with:\n    talman apply --onboard-new-nodes", err, first.Hostname)
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
