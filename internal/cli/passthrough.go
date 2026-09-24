package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/talosctl"
)

// exitCodeError is a child's exit status that talman passes on as its own.
// It carries no message: the child has already said what went wrong.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newTalosctlCmd() *cobra.Command {
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

talman's own flags -- -n, -g, -c and -v -- come first; everything from the first
argument that is not one of them belongs to talosctl, its flags included and
"--" optional, so "talman ctl -e 10.0.0.2 version" reaches talosctl whole.
talosctl's exit status is talman's.`,
		DisableFlagsInUseLine: true,
		// Parsed here rather than by cobra, which rejects a flag it does
		// not know even when it comes before the first argument that is
		// talosctl's.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, raw []string) error {
			nodes, args, help, err := passthroughArgs(raw)
			if err != nil {
				return err
			}

			if help {
				return cmd.Help()
			}

			if len(args) == 0 {
				return errors.New("name a talosctl command to run, e.g. `talman ctl -n <node> logs kubelet`")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			var addrs []string

			if len(nodes) > 0 {
				targets, err := selectNodes(cfg, nodes)
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

	return cmd
}

// passthroughArgs takes talman's own flags off the front of a `talman ctl`
// command line -- -n/--node, -g/--group, -c/--config, -v/--verbose,
// -h/--help -- and
// returns the rest for talosctl. It stops at "--", or at the first argument
// that is not one of them.
func passthroughArgs(raw []string) (nodes, rest []string, help bool, err error) {
	value := func(i int, a, long, short string) (string, int, bool, error) {
		switch {
		case a == long || a == short:
			if i+1 >= len(raw) {
				return "", i, true, fmt.Errorf("%s needs a value", a)
			}

			return raw[i+1], i + 1, true, nil
		case strings.HasPrefix(a, long+"="):
			return strings.TrimPrefix(a, long+"="), i, true, nil
		case strings.HasPrefix(a, short+"="):
			return strings.TrimPrefix(a, short+"="), i, true, nil
		}

		return "", i, false, nil
	}

	for i := 0; i < len(raw); i++ {
		a := raw[i]

		switch a {
		case "--":
			return nodes, raw[i+1:], false, nil
		case "-h", "--help":
			return nil, nil, true, nil
		case "-v", "--verbose":
			opts.verbose = true

			continue
		}

		if v, next, ok, err := value(i, a, "--node", "-n"); err != nil {
			return nil, nil, false, err
		} else if ok {
			nodes = append(nodes, strings.Split(v, ",")...)
			i = next

			continue
		}

		if v, next, ok, err := value(i, a, "--group", "-g"); err != nil {
			return nil, nil, false, err
		} else if ok {
			opts.groups = append(opts.groups, strings.Split(v, ",")...)
			i = next

			continue
		}

		if v, next, ok, err := value(i, a, "--config", "-c"); err != nil {
			return nil, nil, false, err
		} else if ok {
			opts.configFile = v
			i = next

			continue
		}

		return nodes, raw[i:], false, nil
	}

	return nodes, nil, false, nil
}
