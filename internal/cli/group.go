package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// commandGroup is a command that only holds others: `talman secrets`,
// `talman etcd`.
//
// It refuses an argument it does not know. cobra looks for an unknown
// subcommand only at the root, and prints a group's help in place of running
// it, so `talman secrets generat` printed this help and exited 0 -- a typo a
// CI job reads as success. A group with nothing after it still prints its help.
func commandGroup(use, short string, subs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
	}

	refuseUnknownSubcommands(cmd)
	cmd.AddCommand(subs...)

	return cmd
}

// refuseUnknownSubcommands makes a group fail on an argument that names none
// of its subcommands, as the root does, suggestions included.
func refuseUnknownSubcommands(cmd *cobra.Command) {
	cmd.Args = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}

		msg := fmt.Sprintf("unknown command %q for %q", args[0], c.CommandPath())

		if suggestions := c.SuggestionsFor(args[0]); len(suggestions) > 0 {
			msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
		}

		return errors.New(msg)
	}

	cmd.RunE = func(c *cobra.Command, _ []string) error { return c.Help() }

	// What the root uses; left at zero, only a prefix would be suggested.
	cmd.SuggestionsMinimumDistance = 2
}
