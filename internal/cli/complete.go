package cli

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ludwighansson/talman/internal/config"
)

// completeNodes offers hostnames for -n/--node, annotated with each node's
// role.
//
// It loads without validating: completion has to keep working while the config
// is mid-edit, which is exactly when it is most wanted.
func completeNodes(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.LoadNoValidate(config.FindConfig(opts.configFile))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var out []string

	for i := range cfg.Nodes {
		n := &cfg.Nodes[i]
		if n.Hostname == "" || !strings.HasPrefix(n.Hostname, toComplete) {
			continue
		}

		out = append(out, n.Hostname+"\t"+string(n.Role))
	}

	return out, cobra.ShellCompDirectiveNoFileComp
}

// withNodeCompletion wires completeNodes onto a command's node flag, and gives
// every command whose -n takes a list the -g/--group selector beside it.
func withNodeCompletion(cmd *cobra.Command) *cobra.Command {
	f := cmd.Flags().Lookup("node")
	if f == nil {
		return cmd
	}

	_ = cmd.RegisterFlagCompletionFunc("node", completeNodes)

	if f.Value.Type() == "stringSlice" {
		cmd.Flags().StringSliceVarP(&opts.groups, "group", "g", nil,
			"limit to the nodes in these groups, or with this role (repeatable; adds to -n)")
		_ = cmd.RegisterFlagCompletionFunc("group", completeGroups)
	}

	previous := cmd.PreRunE

	cmd.PreRunE = func(c *cobra.Command, args []string) error {
		if err := refuseEmptySelectors(c); err != nil {
			return err
		}

		if previous != nil {
			return previous(c, args)
		}

		return nil
	}

	return cmd
}

// refuseEmptySelectors rejects a -n or -g that was given but names nothing.
//
// Omitting them means every node, so an empty one -- `-n "$NODE"` with NODE
// unset, `--node=` -- read the same way: `talman reset -n "" --yes` wiped the
// whole cluster. A selector that was written down has to select something.
func refuseEmptySelectors(c *cobra.Command) error {
	for _, name := range []string{"node", "group"} {
		f := c.Flags().Lookup(name)
		if f == nil || !f.Changed {
			continue
		}

		values := []string{f.Value.String()}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			values = sv.GetSlice()
		}

		named := false

		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("--%s was given an empty value: name a %s, or leave the flag out for every node",
					name, name)
			}

			named = true
		}

		if !named {
			return fmt.Errorf("--%s was given an empty value: name a %s, or leave the flag out for every node",
				name, name)
		}
	}

	return nil
}

// completeGroups offers the groups the config declares, and the two roles.
func completeGroups(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.LoadNoValidate(config.FindConfig(opts.configFile))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	out := []string{config.GroupControlPlane + "\trole", config.GroupWorker + "\trole"}

	for _, g := range slices.Sorted(maps.Keys(cfg.DeclaredGroups())) {
		if strings.HasPrefix(g, toComplete) {
			out = append(out, g+"\tgroup")
		}
	}

	return out, cobra.ShellCompDirectiveNoFileComp
}
