// Command talman renders Talos machine configurations from explicitly
// referenced patch files and drives talosctl to apply them.
package main

import (
	"os"

	"github.com/ludwighansson/talman/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
