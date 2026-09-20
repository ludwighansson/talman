package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

func newStatusCmd() *cobra.Command {
	var (
		nodes    []string
		parallel int
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report what each node is running, against what the config asks for",
		Long: `Status asks every node what it is: whether it answers at all, the Talos version
and schematic it is running, and the Kubernetes version its kubelet runs. Each
is shown against what the config resolves to, with an arrow marking the drift
that "talman upgrade" or "talman upgrade-k8s" would close.

A node that has not been adopted answers on the maintenance service and is
reported as such rather than as a failure; one that answers nothing is
unreachable. Anything talman could not read is shown as "-", never as the
configured value, because this command exists to say what is actually there.

It reports and always succeeds. "talman health" is the one that passes or
fails.

There is no --extra-flags here: this command composes several talosctl calls
per node rather than driving one, so there is no single invocation for flags to
be forwarded to.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := render.Nodes(cfg, nodes)
			if err != nil {
				return err
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			reports, err := statusReports(cfg, runner(cfg), tc, targets, parallel)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)

			fmt.Fprintln(w, "HOSTNAME\tADDRESS\tROLE\tSTATUS\tTALOS\tKUBERNETES\tSCHEMATIC")

			for _, r := range reports {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Node.Hostname, r.Node.IPAddress, r.Node.Role, r.Mode,
					r.talos(), r.kubernetes(), r.schematic())
			}

			if err := w.Flush(); err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), summarise(reports))

			// A node with a config but no cluster to join never starts
			// serving the Talos API, which looks exactly like a machine that
			// is gone. Only asked when something is quiet, and only said when
			// it is the answer.
			if quiet(reports) && !anyEtcdRunning(runner(cfg), tc, reports) {
				fmt.Fprintln(cmd.OutOrStdout(), "cluster: not bootstrapped — a node with a config but no "+
					"cluster to join stays quiet; run `talman bootstrap`")
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	addParallelFlag(cmd, &parallel, defaultParallel, "how many nodes to ask at once")

	return cmd
}

// nodeReport is one node's live state beside what the config asks of it.
type nodeReport struct {
	Node  *config.Node
	Mode  talosctl.Mode
	State talosctl.NodeState
	K8s   string

	wantTalos      string
	wantSchematic  string
	wantKubernetes string
}

func (r nodeReport) talos() string {
	return drift(r.State.TalosVersion, r.wantTalos)
}

func (r nodeReport) kubernetes() string {
	if r.K8s != "" && k8sUpToDate(r.K8s, r.wantKubernetes) {
		return r.K8s
	}

	return drift(r.K8s, withV(r.wantKubernetes))
}

// schematic compares the running image's schematic with the configured one.
//
// A node installed from something other than a factory image reports no
// schematic at all, which is unknown rather than wrong: there is nothing to
// compare, so it shows as unread instead of as drift against every ID.
func (r nodeReport) schematic() string {
	return drift(short(r.State.SchematicID), short(r.wantSchematic))
}

// drift renders what is running, and what the config wants when the two
// differ. What talman could not read shows as "-" and never as the configured
// value: a node that did not answer must not be printed as if it agreed.
func drift(current, want string) string {
	switch {
	case current == "":
		return "-"
	case current == want:
		return current
	default:
		return current + " → " + want
	}
}

func withV(version string) string {
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}

	return "v" + version
}

// summarise closes the table with the counts an operator would otherwise make
// by eye.
func summarise(reports []nodeReport) string {
	var running, maintenance, unreachable, stale int

	for _, r := range reports {
		switch r.Mode {
		case talosctl.ModeRunning:
			running++

			if !upToDate(r.State, r.wantTalos, r.wantSchematic) || !k8sUpToDate(r.K8s, r.wantKubernetes) {
				stale++
			}
		case talosctl.ModeMaintenance:
			maintenance++
		case talosctl.ModeUnreachable:
			unreachable++
		}
	}

	var parts []string

	for _, part := range []struct {
		n    int
		what string
	}{
		{running, "running"},
		{maintenance, "in maintenance mode"},
		{unreachable, "unreachable"},
	} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.what))
		}
	}

	out := fmt.Sprintf("%d node(s): %s", len(reports), strings.Join(parts, ", "))

	if stale > 0 {
		return fmt.Sprintf("%s; %d not on the configured version", out, stale)
	}

	return out
}

// quiet reports whether anything in the table failed to answer, which is the
// only case worth asking after the cluster for.
func quiet(reports []nodeReport) bool {
	for _, r := range reports {
		if r.Mode == talosctl.ModeUnreachable {
			return true
		}
	}

	return false
}

// anyEtcdRunning reports whether some control plane that answered has etcd,
// which is the difference between a cluster and a set of configured machines.
func anyEtcdRunning(tal *talosctl.Runner, talosconfig string, reports []nodeReport) bool {
	for _, r := range reports {
		if r.Mode != talosctl.ModeRunning || !r.Node.IsControlPlane() {
			continue
		}

		if tal.EtcdRunning(talosconfig, r.Node.IPAddress) {
			return true
		}
	}

	return false
}

// statusReports asks every node what it is running.
//
// Concurrently, because a status report is what gets run when something is
// wrong, and a node that is down costs two dial timeouts before it admits it:
// one dead machine would otherwise hold up the whole table.
//
// What the config wants is resolved first, on this goroutine. The renderer
// caches schematic IDs as it resolves them and is not safe to share.
func statusReports(cfg *config.Config, tal *talosctl.Runner, talosconfig string,
	targets []*config.Node, parallel int,
) ([]nodeReport, error) {
	reports := make([]nodeReport, len(targets))

	// No Open: resolving a schematic needs neither secrets nor talosctl.
	r := &render.Renderer{Cfg: cfg}

	for i, n := range targets {
		ctx, err := r.Context(n)
		if err != nil {
			return nil, err
		}

		reports[i] = nodeReport{
			Node:           n,
			wantTalos:      ctx.Node.TalosVersion,
			wantSchematic:  ctx.Node.SchematicID,
			wantKubernetes: cfg.KubernetesVersion,
		}
	}

	byAddress := make(map[string]*nodeReport, len(reports))
	for i := range reports {
		byAddress[reports[i].Node.IPAddress] = &reports[i]
	}

	// Errors are deliberately swallowed: a node that answers but cannot say
	// what it runs leaves the column empty rather than failing the table.
	_, _ = eachNode(targets, parallel, func(n *config.Node) (struct{}, error) {
		rep := byAddress[n.IPAddress]

		rep.Mode = tal.Mode(talosconfig, n.IPAddress)
		if rep.Mode != talosctl.ModeRunning {
			return struct{}{}, nil
		}

		rep.State, _ = tal.State(talosconfig, n.IPAddress)
		rep.K8s, _ = tal.KubeletVersion(talosconfig, n.IPAddress)

		return struct{}{}, nil
	})

	return reports, nil
}
