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
		offline  bool
		wide     bool
		output   outputFormat
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "List the nodes, and what each one is running",
		Long: `Status lists every node and asks each what it runs -- whether it answers, its
Talos and Kubernetes versions and schematic -- beside what the config wants.
An arrow marks drift; "-" is something talman could not read, never the
configured value. It always succeeds; "talman health" is the one that fails.

--offline asks nothing, and needs no cluster, secrets or talosconfig.
-o json prints the same report for scripts.

More in the README: "Seeing what is out there".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			targets, err := selectNodes(cfg, nodes)
			if err != nil {
				return err
			}

			var (
				tc  string
				tal *talosctl.Runner
			)

			// Nothing is resolved for an offline pass: generating a
			// talosconfig needs the secrets bundle, and a command whose point
			// is that it asks nothing should not ask for that either.
			if !offline {
				if tc, err = ensureTalosconfig(cfg); err != nil {
					return err
				}

				// One runner for the whole command: it remembers which route
				// answered for each node, and a second would pay that
				// discovery again on the path that only runs when something
				// is already down.
				tal = runner(cfg)
			}

			reports, err := statusReports(cfg, tal, tc, targets, parallel)
			if err != nil {
				return err
			}

			if output.json() {
				report := statusJSON{Nodes: make([]nodeJSON, 0, len(reports))}

				for _, r := range reports {
					report.Nodes = append(report.Nodes, r.json(cfg))
				}

				// Asked whether anything is quiet or not: a script reading
				// the field has to be able to tell a bootstrapped cluster
				// from one talman did not ask about.
				if !offline {
					switch askCluster(cfg, tal, tc) {
					case clusterUp:
						report.Bootstrapped = new(true)
					case clusterAbsent:
						report.Bootstrapped = new(false)
					case clusterUnknown:
					}
				}

				return writeJSON(cmd.OutOrStdout(), report)
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)

			header := "HOSTNAME\tADDRESS\tROLE\tSTATUS\tTALOS\tKUBERNETES"
			if wide {
				header += "\tSCHEMATIC"
			}

			fmt.Fprintln(w, header+"\tGROUPS\tPATCHES")

			for _, r := range reports {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t",
					r.Node.Hostname, r.Node.IPAddress, r.Node.Role, r.status(), r.talos(), r.kubernetes())

				if wide {
					fmt.Fprintf(w, "%s\t", r.schematic())
				}

				fmt.Fprintf(w, "%s\t%d\n", groups(r.Node), len(cfg.PatchChain(r.Node)))
			}

			if err := w.Flush(); err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), summarise(reports, offline))

			// A node with a config but no cluster to join never starts
			// serving the Talos API, which looks exactly like a machine that
			// is gone. Only asked when something is quiet, and only said when
			// the control planes answered that there is no etcd -- not when
			// they said nothing at all.
			if !offline && quiet(reports) && askCluster(cfg, tal, tc) == clusterAbsent {
				fmt.Fprintln(cmd.OutOrStdout(), "cluster: not bootstrapped — a node with a config but no "+
					"cluster to join stays quiet; run `talman apply --bootstrap`")
			}

			return nil
		},
	}

	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "limit to these nodes (repeatable)")
	cmd.Flags().BoolVar(&offline, "offline", false,
		"ask nothing: report what the config says, with a dash for what only a node could tell")
	cmd.Flags().BoolVar(&wide, "wide", false, "add the schematic column")
	addOutputFlag(cmd, &output)
	addParallelFlag(cmd, &parallel, defaultParallel, "how many nodes to ask at once")

	return cmd
}

// nodeReport is one node's live state beside what the config asks of it.
type nodeReport struct {
	Node  *config.Node
	Mode  talosctl.Mode
	State talosctl.NodeState
	K8s   string

	// asked is whether the node was contacted at all. Without it an offline
	// pass would be indistinguishable from a node that answered nothing, and
	// the table would report every machine as unreachable when talman simply
	// never knocked.
	asked bool

	wantTalos      string
	wantSchematic  string
	wantKubernetes string
}

// statusJSON is `status -o json`.
type statusJSON struct {
	Nodes []nodeJSON `json:"nodes"`
	// Bootstrapped is whether the control planes answered that etcd is
	// running: absent offline, or when none of them answered.
	Bootstrapped *bool `json:"bootstrapped,omitempty"`
}

type nodeJSON struct {
	Hostname  string   `json:"hostname"`
	IPAddress string   `json:"ipAddress"`
	Role      string   `json:"role"`
	Groups    []string `json:"groups"`
	Patches   int      `json:"patches"`
	// Status is running, maintenance or unreachable; absent offline.
	Status     string    `json:"status,omitempty"`
	Talos      versionAt `json:"talos"`
	Kubernetes versionAt `json:"kubernetes"`
	Schematic  versionAt `json:"schematic"`
}

// versionAt pairs what a node runs with what the config resolves to. Running
// is absent when talman could not read it, or did not ask.
type versionAt struct {
	Running    string `json:"running,omitempty"`
	Configured string `json:"configured"`
}

func (r nodeReport) json(cfg *config.Config) nodeJSON {
	out := nodeJSON{
		Hostname:   r.Node.Hostname,
		IPAddress:  r.Node.IPAddress,
		Role:       string(r.Node.Role),
		Groups:     r.Node.Groups,
		Patches:    len(cfg.PatchChain(r.Node)),
		Talos:      versionAt{Configured: r.wantTalos},
		Kubernetes: versionAt{Configured: withV(r.wantKubernetes)},
		Schematic:  versionAt{Configured: r.wantSchematic},
	}

	if out.Groups == nil {
		out.Groups = []string{}
	}

	if r.asked {
		out.Status = map[talosctl.Mode]string{
			talosctl.ModeRunning:     "running",
			talosctl.ModeMaintenance: "maintenance",
		}[r.Mode]

		if out.Status == "" {
			out.Status = "unreachable"
		}

		out.Talos.Running = r.State.TalosVersion
		out.Kubernetes.Running = withV(r.K8s)
		out.Schematic.Running = r.State.SchematicID
	}

	return out
}

// status is the STATUS cell. Offline it is a dash rather than a guess: what a
// node is doing is the one thing on this row that cannot be read off a config
// file.
func (r nodeReport) status() string {
	if !r.asked {
		return "-"
	}

	return r.Mode.String()
}

func (r nodeReport) talos() string {
	if !r.asked {
		return r.wantTalos
	}

	return drift(r.State.TalosVersion, r.wantTalos)
}

func (r nodeReport) kubernetes() string {
	if !r.asked {
		return withV(r.wantKubernetes)
	}

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
	if !r.asked {
		return short(r.wantSchematic)
	}

	return drift(short(r.State.SchematicID), short(r.wantSchematic))
}

// groups is the GROUPS cell: a node's declared groups, or a dash when it
// belongs to none beyond the two every node is in.
func groups(n *config.Node) string {
	if len(n.Groups) == 0 {
		return "-"
	}

	return strings.Join(n.Groups, ",")
}

// drift renders what is running, and what the config wants when the two
// differ. What talman could not read shows as "-" and never as the configured
// value: a node that did not answer must not be printed as if it agreed.
func drift(current, want string) string {
	switch current {
	case "":
		return "-"
	case want:
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
func summarise(reports []nodeReport, offline bool) string {
	// Nothing about what was or was not asked: the dashes in the table say
	// that already, and saying it twice reads as an apology.
	if offline {
		return fmt.Sprintf("%d node(s)", len(reports))
	}

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

	if tal == nil {
		return reports, nil
	}

	byAddress := make(map[string]*nodeReport, len(reports))
	for i := range reports {
		byAddress[reports[i].Node.IPAddress] = &reports[i]
	}

	// Errors are deliberately swallowed: a node that answers but cannot say
	// what it runs leaves the column empty rather than failing the table.
	_, _ = eachNode(targets, parallel, func(n *config.Node) (struct{}, error) {
		rep := byAddress[n.IPAddress]

		rep.asked = true
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
