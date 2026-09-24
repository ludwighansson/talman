package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/render"
	"github.com/ludwighansson/talman/internal/talosctl"
)

// exitCodeError is a child's exit status that talman passes on as its own.
// It carries no message: the child has already said what went wrong.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newTalosctlCmd() *cobra.Command {
	var nodes []string

	cmd := &cobra.Command{
		Use:     "talosctl [-n node]... [--] <talosctl args>...",
		Aliases: []string{"ctl"},
		Short:   "Run any talosctl command against this cluster's nodes, by hostname",
		Long: `Talosctl runs talosctl with this cluster's talosconfig, and with --nodes set to
the addresses of the nodes named with -n -- by hostname or address, as
everywhere else in talman. Everything else is passed to talosctl as it is:

  talman talosctl -n worker-01 logs kubelet -f
  talman talosctl -n control-01 -n control-02 get members
  talman ctl -- service

It is for everything talman does not wrap: logs, dmesg, get, service, edit and
the rest. Without -n, talosctl reaches the talosconfig's default nodes, which
render sets to every node in the config.

talman's own flags go before the talosctl command; everything from the first
argument that is not one of them on belongs to talosctl, "--" included or not.
talosctl's exit status is talman's.`,
		Args:                  cobra.MinimumNArgs(1),
		DisableFlagsInUseLine: true,
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var addrs []string

			if len(nodes) > 0 {
				targets, err := render.Nodes(cfg, nodes)
				if err != nil {
					return err
				}

				for _, n := range targets {
					addrs = append(addrs, n.IPAddress)
				}
			}

			tc, err := ensureTalosconfig(cfg)
			if err != nil {
				return err
			}

			argv := []string{"--talosconfig", tc}
			if len(addrs) > 0 {
				argv = append(argv, "--nodes", strings.Join(addrs, ","))
			}

			argv = append(argv, args...)

			if err := runner(cfg).Stream(argv...); err != nil {
				var status *talosctl.StatusError
				if errors.As(err, &status) {
					return exitCodeError{status.Code}
				}

				return err
			}

			return nil
		},
	}

	// Stop at the first argument that is not talman's, so talosctl's own
	// flags reach it without a "--" and without talman rejecting them.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().StringSliceVarP(&nodes, "node", "n", nil, "run against these nodes (repeatable)")

	return cmd
}
