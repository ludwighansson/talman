package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ludwighansson/talman/internal/config"
)

func newVersionCmd() *cobra.Command {
	var output outputFormat

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the talman and talosctl versions",
		Long: `Print talman's version and that of the talosctl it will use.

talman delegates everything Talos-shaped to talosctl, so which talosctl is on
PATH is part of the answer to "what will this do": talosctl parses and
validates the config it is handed, and one older than the document kinds in
play will reject them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			// Prefer the configured binary, but stay useful outside a cluster
			// directory where there is no config to read.
			bin := config.DefaultTalosctl
			if cfg, err := config.LoadNoValidate(config.FindConfig(opts.configFile)); err == nil {
				bin = cfg.Talosctl
			}

			tal := runnerFor(bin)

			version, err := tal.ClientVersion()

			if output.json() {
				report := struct {
					Talman   string `json:"talman"`
					Talosctl struct {
						Binary  string `json:"binary"`
						Version string `json:"version,omitempty"`
						Error   string `json:"error,omitempty"`
					} `json:"talosctl"`
				}{Talman: Version}

				report.Talosctl.Binary = bin
				report.Talosctl.Version = version

				if err != nil {
					report.Talosctl.Error = err.Error()
				}

				return writeJSON(out, report)
			}

			fmt.Fprintf(out, "talman   %s\n", Version)

			if err != nil {
				fmt.Fprintf(out, "talosctl %s (not usable: %v)\n", bin, err)

				return nil
			}

			fmt.Fprintf(out, "talosctl %s\n", version)

			return nil
		},
	}

	addOutputFlag(cmd, &output)

	return cmd
}
