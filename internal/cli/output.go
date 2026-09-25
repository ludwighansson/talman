package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"
)

// Output formats a reporting command can print in.
const (
	outputText = "text"
	outputJSON = "json"
)

// outputFormat is the value of -o/--output, checked when it is set.
type outputFormat string

func (f *outputFormat) String() string { return string(*f) }
func (f *outputFormat) Type() string   { return "format" }

func (f *outputFormat) Set(v string) error {
	if !slices.Contains([]string{outputText, outputJSON}, v) {
		return fmt.Errorf("must be %s or %s", outputText, outputJSON)
	}

	*f = outputFormat(v)

	return nil
}

func (f outputFormat) json() bool { return f == outputJSON }

// addOutputFlag registers -o/--output.
//
// The text form is for reading, the JSON form for scripts.
func addOutputFlag(cmd *cobra.Command, target *outputFormat) {
	*target = outputText
	cmd.Flags().VarP(target, "output", "o", "output format: text or json")

	_ = cmd.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{outputText, outputJSON}, cobra.ShellCompDirectiveNoFileComp
	})
}

// writeJSON prints v as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)

	return enc.Encode(v)
}
