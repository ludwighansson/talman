// Package cli implements the talman command line.
package cli

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/interrupt"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// Version is set at build time by goreleaser.
var Version = "dev"

// errChanged is what a command returns when --detailed-exit-code is set and
// the run changed something, or would have. It is a result rather than a
// failure, so Execute turns it into an exit code and prints nothing.
var errChanged = errors.New("changes")

// addDetailedExitCode registers the flag that turns "did this change
// anything?" into something a CI job can branch on without parsing output.
func addDetailedExitCode(cmd *cobra.Command, target *bool) {
	cmd.Flags().BoolVar(target, "detailed-exit-code", false,
		"exit 2 when something changed or would change, 0 when nothing did, 1 on error")
}

type globals struct {
	configFile string
	verbose    bool
}

var opts globals

// Execute runs the root command and returns a process exit code.
//
// 0 is success, 1 a failure of any kind -- a usage error included -- 2 a
// --detailed-exit-code run that changed something, and 130 a run stopped by
// SIGINT or SIGTERM, which is reported whatever else the command returned.
func Execute() int {
	defer interrupt.Watch()()

	return run(os.Args[1:])
}

// run executes one command line and maps its outcome to an exit code.
func run(args []string) int {
	root := newRootCmd()
	root.SetArgs(args)

	err := root.ExecuteContext(interrupt.Context())

	if interrupt.Interrupted() {
		if err != nil && !errors.Is(err, errChanged) {
			fmt.Fprintln(os.Stderr, "error: "+err.Error())
		}

		fmt.Fprintln(os.Stderr, "interrupted")

		return interrupt.ExitCode
	}

	if err != nil {
		if errors.Is(err, errChanged) {
			return 2
		}

		// A passed-through talosctl has said what went wrong already; its
		// status is the answer.
		var exit exitCodeError
		if errors.As(err, &exit) {
			return exit.code
		}

		// SilenceErrors is set, so this is the only place any error is
		// printed, cobra's own usage errors included.
		fmt.Fprintln(os.Stderr, "error: "+err.Error())

		return 1
	}

	return 0
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "talman",
		Short: "Manage Talos clusters and their configuration patches",
		Long: `talman renders Talos machine configurations from explicitly referenced patch
files and drives talosctl to apply them.

Patches are addressed by path, never by directory convention. Each node belongs
to the reserved groups "all" and its role, plus any groups it declares, and the
patches for those groups are applied in that order.

talman never links against the Talos API. Everything that touches a cluster
goes through the talosctl binary, so a new Talos release needs no talman
release.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}

	cmd.PersistentFlags().StringVarP(&opts.configFile, "config", "c", "",
		"path to the talman config (default $"+config.EnvConfig+", else "+config.DefaultFileName+")")
	cmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false,
		"echo each talosctl invocation")
	addMetricsFlags(cmd)

	for _, sub := range []*cobra.Command{
		newRenderCmd(),
		newValidateCmd(),
		newPatchesCmd(),
		newStatusCmd(),
		newSecretsCmd(),
		newSchematicCmd(),
		newImageCmd(),
		recorded(newApplyCmd()),
		recorded(newBootstrapCmd()),
		newKubeconfigCmd(),
		recorded(newUpgradeCmd()),
		recorded(newUpgradeK8sCmd()),
		recorded(newHealthCmd()),
		newDashboardCmd(),
		newTalosctlCmd(),
		recorded(newResetCmd()),
		newVersionCmd(),
	} {
		cmd.AddCommand(withNodeCompletion(sub))

		for _, nested := range sub.Commands() {
			withNodeCompletion(nested)
		}
	}

	return cmd
}

// loadConfig reads and validates the config file.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(config.FindConfig(opts.configFile))
	if err == nil {
		currentRun.SetCluster(cfg.ClusterName)
	}

	return cfg, err
}

// runner returns a talosctl runner honouring the config and -v.
func runner(cfg *config.Config) *talosctl.Runner {
	return runnerFor(cfg.Talosctl)
}

// runnerFor is runner for callers that have a binary but no config.
func runnerFor(bin string) *talosctl.Runner {
	r := talosctl.New(bin)
	r.Verbose = opts.verbose

	return r
}

// newRenderer builds an opened Renderer. The caller must Close it.
func newRenderer(cfg *config.Config, submit bool, log *os.File) (*render.Renderer, error) {
	r := &render.Renderer{
		Cfg:    cfg,
		Tal:    runner(cfg),
		Submit: submit,
		Log:    log,
	}

	if err := r.Open(); err != nil {
		return nil, err
	}

	return r, nil
}

// ensureTalosconfig returns the talosconfig every cluster-facing command needs,
// generating it first when it is not there.
//
// It used to fail with "run `talman render` first", which asked the operator
// to run a command talman can run itself: the file is derived entirely from
// the secrets bundle and the node list, both of which are already in hand, and
// nothing about producing it touches the cluster. A fresh checkout of a
// cluster directory has no output directory at all -- it is gitignored -- so
// that error met everyone who cloned one and reached for `kubeconfig`.
//
// Only a missing file is generated. An existing one is left exactly as it is,
// because it is also the file an operator may have pointed at a bastion or a
// different endpoint on purpose; `render` is the command that rewrites it.
func ensureTalosconfig(cfg *config.Config) (string, error) {
	// Every command that reaches a cluster passes through here, which makes
	// it the place to find out that the binary doing the reaching is too old
	// -- before an operation stops halfway with talosctl's words about a flag
	// it does not have.
	if err := runner(cfg).Ensure(); err != nil {
		return "", err
	}

	path := cfg.TalosconfigPath()

	if _, err := os.Stat(path); err == nil {
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "no talosconfig at %s; generating one from %s\n",
		render.Rel(path), cfg.SecretFile)

	r, err := newRenderer(cfg, false, os.Stderr)
	if err != nil {
		return "", err
	}

	defer r.Close()

	written, err := r.WriteTalosconfig()
	if err != nil {
		return "", err
	}

	fmt.Fprintf(os.Stderr, "wrote %s\n", render.Rel(written))

	return written, nil
}

// replayFlags spells out the flags a command was run with, so that a resume
// hint runs the same command over the rest of the nodes.
//
// Every flag that was set, not a list of the ones that seemed to matter: a
// hint that drops --dry-run resumes as a real apply, and one that drops
// --wipe-disk or --mode=staged runs a different operation from the one that
// stopped. except names flags the hint handles itself -- the node list -- or
// that should be asked again, like --yes.
func replayFlags(cmd *cobra.Command, except ...string) []string {
	var out []string

	cmd.Flags().Visit(func(f *pflag.Flag) {
		if slices.Contains(except, f.Name) {
			return
		}

		if sv, ok := f.Value.(pflag.SliceValue); ok {
			items := sv.GetSlice()
			if len(items) == 0 {
				out = append(out, "--"+f.Name+"=")
			}

			for _, v := range items {
				out = append(out, "--"+f.Name+"="+shellQuote(v))
			}

			return
		}

		if f.Value.Type() == "bool" && f.Value.String() == "true" {
			out = append(out, "--"+f.Name)

			return
		}

		out = append(out, "--"+f.Name+"="+shellQuote(f.Value.String()))
	})

	return out
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:=,@+%-]+$`)

// shellQuote makes a value safe to paste into a shell.
func shellQuote(v string) string {
	if shellSafe.MatchString(v) {
		return v
	}

	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// resumeHint names the nodes a stopped pass did not get to, and spells out the
// command that picks up where it left off.
//
// A node-by-node command stops at the first failure, which leaves the operator
// holding a half-finished cluster and a scroll-back to read the remainder out
// of. The node it failed on is part of the remainder: its own work did not
// finish either.
//
// done is the past participle for the message ("applied", "reset"); command is
// what to type to resume.
func resumeHint(command, done string, remaining []*config.Node, flags ...string) string {
	names := make([]string, 0, len(remaining))

	var resume strings.Builder

	resume.WriteString("talman " + command)

	for _, n := range remaining {
		names = append(names, n.Hostname)
		resume.WriteString(" -n " + n.Hostname)
	}

	for _, f := range flags {
		resume.WriteString(" " + f)
	}

	return fmt.Sprintf("  %d node(s) were not %s: %s\n  continue with: %s",
		len(names), done, strings.Join(names, ", "), resume.String())
}

// renderContext is the slice of template context these commands report on.
type renderContext struct {
	SchematicID    string
	InstallerImage string
}

// printPerNode prints one value per node: a two-column table, or with
// -o json a list of {"hostname": ..., field: value}.
func printPerNode(cmd *cobra.Command, nodes []string, submit bool, output outputFormat,
	field string, value func(renderContext) string,
) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	targets, err := render.Nodes(cfg, nodes)
	if err != nil {
		return err
	}

	// No Open: resolving a schematic needs neither secrets nor talosctl.
	r := &render.Renderer{Cfg: cfg, Submit: submit}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	rows := []map[string]string{}

	for _, n := range targets {
		ctx, err := r.Context(n)
		if err != nil {
			return err
		}

		v := value(renderContext{
			SchematicID:    ctx.Node.SchematicID,
			InstallerImage: ctx.Node.InstallerImage,
		})

		if output.json() {
			rows = append(rows, map[string]string{"hostname": n.Hostname, field: v})

			continue
		}

		fmt.Fprintf(w, "%s\t%s\n", n.Hostname, v)
	}

	if output.json() {
		return writeJSON(cmd.OutOrStdout(), map[string]any{"nodes": rows})
	}

	return w.Flush()
}

// addExtraFlags registers the escape hatch every talosctl-invoking command
// carries.
//
// talman models only the talosctl flags it has an opinion about, which leaves
// everything else -- a new flag in a Talos release, a niche one, anything the
// author did not anticipate -- unreachable. Rather than grow the surface to
// chase talosctl's, each command forwards whatever it is given verbatim.
//
// Repeatable rather than a single split string: values contain commas, spaces
// and quotes, and splitting them here would mangle exactly the arguments that
// most need passing through.
func addExtraFlags(cmd *cobra.Command, target *[]string) {
	cmd.Flags().StringArrayVar(target, "extra-flags", nil,
		"extra flag passed verbatim to the underlying talosctl command (repeatable)")
}
