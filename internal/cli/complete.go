package cli

import (
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"

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

	return cmd
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
