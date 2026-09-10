// Package cli implements the talman command line.
package cli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// Version is set at build time by goreleaser.
var Version = "dev"

type globals struct {
	configFile string
	verbose    bool
}

var opts globals

// Execute runs the root command and returns a process exit code.
func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed usage errors; everything else is ours.
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
		"path to the talman config (default "+config.DefaultFileName+")")
	cmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false,
		"echo each talosctl invocation")

	for _, sub := range []*cobra.Command{
		newRenderCmd(),
		newValidateCmd(),
		newPatchesCmd(),
		newNodesCmd(),
		newSecretsCmd(),
		newSchematicCmd(),
		newImageCmd(),
		newDiffCmd(),
		newApplyCmd(),
		newBootstrapCmd(),
		newKubeconfigCmd(),
		newUpgradeCmd(),
		newUpgradeK8sCmd(),
		newHealthCmd(),
		newResetCmd(),
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
	return config.Load(config.FindConfig(opts.configFile))
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

// requireTalosconfig returns the generated talosconfig path, or an actionable
// error when it has not been rendered yet.
func requireTalosconfig(cfg *config.Config) (string, error) {
	path := cfg.TalosconfigPath()

	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no talosconfig at %s: run `talman render` first", path)
		}

		return "", err
	}

	return path, nil
}

// renderContext is the slice of template context these commands report on.
type renderContext struct {
	SchematicID    string
	InstallerImage string
}

func printPerNode(cmd *cobra.Command, nodes []string, submit bool,
	emit func(*tabwriter.Writer, string, renderContext),
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

	for _, n := range targets {
		ctx, err := r.Context(n)
		if err != nil {
			return err
		}

		emit(w, n.Hostname, renderContext{
			SchematicID:    ctx.Node.SchematicID,
			InstallerImage: ctx.Node.InstallerImage,
		})
	}

	return w.Flush()
}
