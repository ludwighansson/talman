package cli

import (
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

// withNodeCompletion wires completeNodes onto a command's node flag.
func withNodeCompletion(cmd *cobra.Command) *cobra.Command {
	if cmd.Flags().Lookup("node") != nil {
		_ = cmd.RegisterFlagCompletionFunc("node", completeNodes)
	}

	return cmd
}
