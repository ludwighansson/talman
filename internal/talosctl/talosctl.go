// Package talosctl shells out to the talosctl binary.
//
// This is talman's entire Talos integration. Nothing else in the tool knows
// what a machine config document is, which is the point: when Talos moves
// fields between document kinds, talosctl absorbs it and talman does not
// change.
package talosctl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/ludwighansson/talman/internal/interrupt"
)

// Runner invokes a talosctl binary.
type Runner struct {
	// Bin is the binary name or path.
	Bin string
	// Verbose echoes each command to Trace before running it.
	Verbose bool
	// Trace receives command echoes when Verbose is set.
	Trace io.Writer

	// routes remembers which way each node answered, so a pass does not pay
	// a dial timeout per call to rediscover it.
	routeMu sync.Mutex
	routes  map[string]bool

	ensureOnce sync.Once
	ensureErr  error
}

// New returns a Runner for the given binary.
func New(bin string) *Runner {
	if bin == "" {
		bin = "talosctl"
	}

	return &Runner{Bin: bin, Trace: os.Stderr}
}

// ExitError carries a failed talosctl invocation's output.
type ExitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}

	return fmt.Sprintf("talosctl %s: %s", e.subcommand(), msg)
}

func (e *ExitError) subcommand() string {
	return subcommand(e.Args)
}

// subcommand is the leading non-flag words of an invocation. The full argv of
// a gen config call is hundreds of characters of temp paths, and an apply
// carries absolute config paths; printing either buries the message that
// actually matters. Use -v to see the whole command.
func subcommand(args []string) string {
	var words []string

	// Skip leading global flags and their values rather than stopping at
	// them: ConfigEndpoint and ConfigNode both build argv starting with
	// --talosconfig, and breaking here reported "(no subcommand)".
	for i := 0; i < len(args); i++ {
		a := args[i]

		if strings.HasPrefix(a, "-") {
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++ // the flag's value
			}

			continue
		}

		words = append(words, a)

		if len(words) == 2 {
			break
		}
	}

	if len(words) == 0 {
		return "(no subcommand)"
	}

	return strings.Join(words, " ")
}

func (e *ExitError) Unwrap() error { return e.Err }

// MinVersion is the oldest talosctl talman drives.
//
// Not a guess at what happens to work: talman relies on flags and resources
// that arrived over time -- `version --insecure` for the maintenance probe,
// `get services` for whether etcd is up, `reset --wipe-labels`. An older
// binary fails in the middle of an operation with talosctl's own words about
// an unknown flag, which is a worse way to learn this than being told.
//
// It is also the version CI tests against, so it is a claim talman keeps
// rather than one it hopes for.
const MinVersion = "v1.14.0"

// Ensure reports a helpful error if the binary is not usable.
//
// Checked once per Runner: it costs a process, and a pass over fifty nodes
// should not pay for it fifty times.
func (r *Runner) Ensure() error {
	r.ensureOnce.Do(func() { r.ensureErr = r.check() })

	return r.ensureErr
}

func (r *Runner) check() error {
	if _, err := exec.LookPath(r.Bin); err != nil {
		return fmt.Errorf("talosctl not found on PATH as %q: talman drives Talos entirely through it "+
			"(install it, or set `talosctl:` in talman.yaml to a path)", r.Bin)
	}

	version, err := r.ClientVersion()
	if err != nil {
		// Unreadable is not too old. Something that answers oddly to
		// `version --client` may still do everything else correctly, and
		// refusing to run on that basis would be worse than the risk.
		return nil //nolint:nilerr // deliberate: an unreadable version is not evidence of an old one
	}

	if olderThan(version, MinVersion) {
		return fmt.Errorf("talosctl %s is too old: talman needs %s or newer "+
			"(it uses `version --insecure`, `get services` and `reset --wipe-labels`)",
			version, MinVersion)
	}

	return nil
}

// olderThan compares two v-prefixed versions on major, minor and patch.
//
// Anything it cannot parse is not old: a build tagged something unexpected --
// a distribution's own string, a development build -- should be allowed to
// run rather than stopped by a comparison talman could not make.
func olderThan(version, minimum string) bool {
	have, ok := semver(version)
	if !ok {
		return false
	}

	want, ok := semver(minimum)
	if !ok {
		return false
	}

	for i := range have {
		if have[i] != want[i] {
			return have[i] < want[i]
		}
	}

	return false
}

// semver pulls major, minor and patch out of a tag, ignoring any pre-release
// suffix: v1.15.0-alpha.1 is newer than v1.14.0, which is the only question
// being asked.
func semver(tag string) ([3]int, bool) {
	var out [3]int

	tag = strings.TrimPrefix(strings.TrimSpace(tag), "v")
	tag, _, _ = strings.Cut(tag, "-")
	tag, _, _ = strings.Cut(tag, "+")

	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return out, false
	}

	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}

		out[i] = n
	}

	return out, true
}

// Output runs talosctl and returns stdout. stderr is captured and surfaced
// only if the command fails: talosctl writes progress lines like
// "generating PKI and tokens" to stderr on success.
func (r *Runner) Output(args ...string) ([]byte, error) {
	cmd := interrupt.Command(r.Bin, args...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	r.echo(args)

	if err := cmd.Run(); err != nil {
		return nil, &ExitError{Args: args, Stderr: stderr.String(), Err: err}
	}

	return stdout.Bytes(), nil
}

// Combined runs talosctl and returns stdout and stderr interleaved, for a
// caller that will print the result itself.
//
// Output is the wrong tool for that: it keeps stderr back unless the command
// fails, and talosctl's action tracker -- every progress and completion line
// an upgrade or a reset produces -- writes there. Capturing with Output left a
// node's block empty, so a parallel pass printed a header per node and nothing
// underneath it.
//
// The bytes come back on failure too. What a command managed to say before it
// died is usually the explanation.
func (r *Runner) Combined(args ...string) ([]byte, error) {
	cmd := interrupt.Command(r.Bin, args...)

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf

	r.echo(args)

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return buf.Bytes(), fmt.Errorf("talosctl %s exited with status %d", subcommand(args), exitErr.ExitCode())
		}

		return buf.Bytes(), fmt.Errorf("talosctl %s: %w", subcommand(args), err)
	}

	return buf.Bytes(), nil
}

// Stream runs talosctl with the caller's stdio attached, for interactive and
// long-running commands where progress matters more than capture.
func (r *Runner) Stream(args ...string) error {
	cmd := interrupt.Command(r.Bin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	r.echo(args)

	if err := cmd.Run(); err != nil {
		// Named the same way as a captured failure: the streamed output has
		// already shown the operator what went wrong, so repeating the argv
		// only buries it.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("talosctl %s exited with status %d", subcommand(args), exitErr.ExitCode())
		}

		return fmt.Errorf("talosctl %s: %w", subcommand(args), err)
	}

	return nil
}

func (r *Runner) echo(args []string) {
	if !r.Verbose || r.Trace == nil {
		return
	}

	fmt.Fprintf(r.Trace, "+ %s %s\n", r.Bin, strings.Join(args, " "))
}

// GenSecretsOptions configures `talosctl gen secrets`.
type GenSecretsOptions struct {
	// TalosVersion selects the version contract, which decides among other
	// things whether the bundle gets a secretbox or an AES-CBC key.
	TalosVersion string
	// FromControlPlaneConfig extracts the bundle from an existing machine
	// config instead of generating new material -- the adoption path for a
	// cluster that already exists.
	FromControlPlaneConfig string
	// ExtraArgs are appended verbatim.
	ExtraArgs []string
}

// GenSecrets generates a secrets bundle and returns it as YAML.
func (r *Runner) GenSecrets(opts GenSecretsOptions) ([]byte, error) {
	args := []string{"gen", "secrets", "--output-file", "-"}

	if opts.TalosVersion != "" {
		args = append(args, "--talos-version", opts.TalosVersion)
	}

	if opts.FromControlPlaneConfig != "" {
		args = append(args, "--from-controlplane-config", opts.FromControlPlaneConfig)
	}

	args = append(args, opts.ExtraArgs...)

	return r.Output(args...)
}

// GenConfigOptions configures one `talosctl gen config` invocation.
type GenConfigOptions struct {
	ClusterName       string
	Endpoint          string
	SecretsFile       string
	TalosVersion      string
	KubernetesVersion string
	InstallImage      string
	// OutputType is controlplane, worker or talosconfig.
	OutputType string
	// Patches are absolute paths, applied in order.
	Patches []string
	// WithDocs and WithExamples control the commented output. Both default
	// off in talman: the rendered files are build artefacts, and the comments
	// triple their size for no review value.
	WithDocs     bool
	WithExamples bool
	// ExtraArgs are appended verbatim, as the escape hatch for talosctl flags
	// talman does not model.
	ExtraArgs []string
}

// GenConfig runs `talosctl gen config` and returns the generated document on
// stdout.
//
// One invocation produces a node's complete config: the base for the version
// contract plus every patch, merged by Talos itself. This is Sidero's own
// documented reproducible-configuration workflow.
func (r *Runner) GenConfig(opts GenConfigOptions) ([]byte, error) {
	args := []string{"gen", "config", opts.ClusterName, opts.Endpoint}

	if opts.SecretsFile != "" {
		args = append(args, "--with-secrets", opts.SecretsFile)
	}

	if opts.TalosVersion != "" {
		args = append(args, "--talos-version", opts.TalosVersion)
	}

	if opts.KubernetesVersion != "" {
		args = append(args, "--kubernetes-version", opts.KubernetesVersion)
	}

	if opts.InstallImage != "" {
		args = append(args, "--install-image", opts.InstallImage)
	}

	args = append(args,
		fmt.Sprintf("--with-docs=%t", opts.WithDocs),
		fmt.Sprintf("--with-examples=%t", opts.WithExamples),
	)

	for _, p := range opts.Patches {
		args = append(args, "--config-patch", "@"+p)
	}

	args = append(args, "--output-types", opts.OutputType, "--output", "-")
	args = append(args, opts.ExtraArgs...)

	return r.Output(args...)
}

// ConfigEndpoint sets the endpoint list on a talosconfig file.
//
// `gen config` emits `endpoints: []`, and the Go option that would set them is
// not exposed on the CLI, so this is a required second step rather than a
// convenience.
func (r *Runner) ConfigEndpoint(talosconfig string, endpoints []string) error {
	args := append([]string{"--talosconfig", talosconfig, "config", "endpoint"}, endpoints...)
	_, err := r.Output(args...)

	return err
}

// ConfigNode sets the default node list on a talosconfig file.
func (r *Runner) ConfigNode(talosconfig string, nodes []string) error {
	args := append([]string{"--talosconfig", talosconfig, "config", "node"}, nodes...)
	_, err := r.Output(args...)

	return err
}

// Validate runs `talosctl validate` against a rendered machine config.
func (r *Runner) Validate(file, mode string) error {
	_, err := r.Output("validate", "--config", file, "--mode", mode)

	return err
}

// ClientVersion returns the talosctl client version tag.
//
// There is no --short flag, so this parses the Tag line out of the block
// `talosctl version --client` prints:
//
//	Client:
//	        Tag:         v1.14.0
//	        SHA:         undefined
func (r *Runner) ClientVersion() (string, error) {
	out, err := r.Output("version", "--client")
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(out), "\n") {
		field := strings.TrimSpace(line)

		tag, ok := strings.CutPrefix(field, "Tag:")
		if !ok {
			continue
		}

		if tag = strings.TrimSpace(tag); tag != "" {
			return tag, nil
		}
	}

	// Better to show something unparsed than to fail a diagnostic command.
	return strings.TrimSpace(string(out)), nil
}
