package talosctl

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/interrupt"
)

// NodeState is what talman can learn about a running node through talosctl.
//
// Fields are best-effort: a node in maintenance mode, or one running a Talos
// old enough to lack a resource, leaves them empty rather than failing. The
// callers treat "unknown" as "cannot prove it is up to date".
type NodeState struct {
	// TalosVersion is the tag the node is running, v-prefixed.
	TalosVersion string
	// SchematicID is the Image Factory schematic the running image was built
	// from, as reported by the schematic extension.
	SchematicID string
}

// State reads a node's running Talos version and schematic.
//
// Asked both ways, like everything else about a single node: see askNode.
func (r *Runner) State(talosconfig, node string) (NodeState, error) {
	var state NodeState

	raw, err := r.askNode(talosconfig, node, "version")
	if err != nil {
		return state, err
	}

	state.TalosVersion = parseServerTag(string(raw))

	// A node with no schematic extension (not installed from a factory image)
	// is not an error; it just cannot be compared on schematic.
	ext, err := r.askNode(talosconfig, node, "get", "extensions", "--output", "yaml")
	if err == nil {
		state.SchematicID = parseSchematicID(ext)
	}

	return state, nil
}

// parseServerTag pulls the server-side tag out of `talosctl version`.
//
// The output carries a Client block and a Server block, each with a Tag, so
// taking the first match would report the local talosctl version as if it
// were the node's:
//
//	Client:
//	        Tag:         v1.14.0
//	Server:
//	        NODE:        10.0.0.11
//	        Tag:         v1.13.5
func parseServerTag(out string) string {
	inServer := false

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)

		switch {
		case strings.HasPrefix(trimmed, "Server:"):
			inServer = true
		case strings.HasPrefix(trimmed, "Client:"):
			inServer = false
		case inServer && strings.HasPrefix(trimmed, "Tag:"):
			tag := strings.TrimSpace(strings.TrimPrefix(trimmed, "Tag:"))
			if tag != "" && !strings.HasPrefix(tag, "v") {
				tag = "v" + tag
			}

			return tag
		}
	}

	return ""
}

// parseSchematicID finds the schematic extension in `talosctl get extensions`.
//
// The walk is deliberately structural rather than tied to a field path: the
// resource layout is Talos', not talman's, and a version bump that nests it
// differently should degrade to "unknown" rather than to a wrong answer.
func parseSchematicID(out []byte) string {
	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc any

		if err := dec.Decode(&doc); err != nil {
			return ""
		}

		if id := findSchematic(doc); id != "" {
			return id
		}
	}
}

func findSchematic(node any) string {
	switch v := node.(type) {
	case map[string]any:
		if name, _ := v["name"].(string); name == "schematic" {
			if version, ok := v["version"].(string); ok && version != "" {
				return version
			}
		}

		for _, child := range v {
			if id := findSchematic(child); id != "" {
				return id
			}
		}
	case []any:
		for _, child := range v {
			if id := findSchematic(child); id != "" {
				return id
			}
		}
	}

	return ""
}

// KubeletVersion reads the Kubernetes version a node's kubelet runs.
//
// Talos publishes no "this node runs Kubernetes X" resource; what it has is
// the kubelet's image, and the tag on that image is the version. KubeletSpec
// is the spec the node is actually running the kubelet from -- upgrade-k8s
// rewrites it and Talos restarts the kubelet -- so it moves when the node
// moves, which is what a skip decision needs.
//
// An empty version means talman could not tell, never that the node is at
// some default: callers must read it as "cannot prove it is up to date".
func (r *Runner) KubeletVersion(talosconfig, node string) (string, error) {
	out, err := r.askNode(talosconfig, node, "get", "kubeletspec", "--output", "yaml")
	if err != nil {
		return "", err
	}

	return parseKubeletVersion(out), nil
}

// kubeletImage matches the tag on a kubelet image reference, e.g.
// ghcr.io/siderolabs/kubelet:v1.37.0. A digest may follow the tag when the
// image is pinned, and is not part of the version.
var kubeletImage = regexp.MustCompile(`(?:^|/)kubelet:(v?\d+\.\d+\.\d+[^\s@]*)`)

// parseKubeletVersion finds the kubelet version in `talosctl get kubeletspec`.
//
// Like parseSchematicID this walks the document rather than indexing a field
// path: the resource layout is Talos', and a release that nests the image
// differently should degrade to "unknown" rather than to a wrong answer. Only
// the kubelet's own image can match, so finding it anywhere is enough.
func parseKubeletVersion(out []byte) string {
	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc any

		if err := dec.Decode(&doc); err != nil {
			return ""
		}

		if v := findKubeletVersion(doc); v != "" {
			return v
		}
	}
}

func findKubeletVersion(node any) string {
	switch v := node.(type) {
	case string:
		if m := kubeletImage.FindStringSubmatch(v); m != nil {
			return vPrefixed(m[1])
		}
	case map[string]any:
		for _, child := range v {
			if tag := findKubeletVersion(child); tag != "" {
				return tag
			}
		}
	case []any:
		for _, child := range v {
			if tag := findKubeletVersion(child); tag != "" {
				return tag
			}
		}
	}

	return ""
}

func vPrefixed(version string) string {
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}

	return "v" + version
}

// askNode runs a read-only talosctl command about one node, pinned to that
// node first and routed through the talosconfig's endpoints second.
//
// Neither route answers on its own, which is the whole reason for trying both.
// Pinning is the only way to reach a node in maintenance mode, or any node
// while the control planes that would proxy for it are down -- a cluster being
// built, or torn down. Going through the endpoints is the only way to reach a
// node whose API is not exposed beyond the cluster network, which is how a
// worker is commonly firewalled: talman applies its config through a control
// plane, so it has to be able to ask after it the same way.
//
// Whichever answered is remembered for the node, so a wait that polls every
// five seconds pays the discovery once instead of a dial timeout each time.
func (r *Runner) askNode(talosconfig, node string, args ...string) ([]byte, error) {
	order := []bool{false, true}
	if r.prefersProxy(node) {
		order = []bool{true, false}
	}

	var first error

	for i, viaProxy := range order {
		base := []string{"--talosconfig", talosconfig}

		if !viaProxy {
			base = append(base, "--endpoints", node)
		}

		base = append(base, "--nodes", node)

		out, err := r.Output(append(base, args...)...)
		if err == nil {
			r.rememberRoute(node, viaProxy)

			return out, nil
		}

		if i == 0 {
			first = err
		}
	}

	return nil, first
}

func (r *Runner) prefersProxy(node string) bool {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()

	return r.routes[node]
}

// NodeArgs are the leading arguments that reach node the way it last
// answered: pinned to its own address, or through the talosconfig's
// endpoints. A command that follows a probe -- a snapshot, a read -- goes the
// way the probe proved works, rather than through endpoints that may be the
// very thing that is down.
func (r *Runner) NodeArgs(talosconfig, node string) []string {
	args := []string{"--talosconfig", talosconfig}

	if !r.prefersProxy(node) {
		args = append(args, "--endpoints", node)
	}

	return append(args, "--nodes", node)
}

func (r *Runner) rememberRoute(node string, viaProxy bool) {
	r.routeMu.Lock()
	defer r.routeMu.Unlock()

	if r.routes == nil {
		r.routes = map[string]bool{}
	}

	r.routes[node] = viaProxy
}

// Mode is how a node's Talos API answers.
type Mode int

// The three answers a node can give.
const (
	// ModeUnreachable is neither API answering: the machine is down, or
	// nothing talman can say applies to it.
	ModeUnreachable Mode = iota
	// ModeRunning is cluster PKI: the node has a config and has joined.
	ModeRunning
	// ModeMaintenance is the maintenance service: the node is up and waiting
	// for a config, having never had one or having been reset.
	ModeMaintenance
)

func (m Mode) String() string {
	switch m {
	case ModeRunning:
		return "running"
	case ModeMaintenance:
		return "maintenance mode"
	default:
		return "unreachable"
	}
}

// Mode probes one node to find out which API it answers on.
//
// The two calls are the two ways to talk to Talos, asked in the order that
// makes a healthy cluster cheap: a running node answers the first call, a node
// in maintenance refuses it and answers the maintenance probe, and a machine
// that is down costs every route a dial timeout before saying so -- the two
// askNode tries plus the maintenance one.
//
// --insecure goes after the subcommand: it is a flag on `version`, not a
// global, and talosctl rejects the invocation outright when it comes first.
func (r *Runner) Mode(talosconfig, node string) Mode {
	if _, err := r.askNode(talosconfig, node, "version"); err == nil {
		return ModeRunning
	}

	if r.inMaintenance(node) {
		return ModeMaintenance
	}

	return ModeUnreachable
}

// inMaintenance asks the maintenance service directly, with no fallback:
// it is reached at the node or not at all, because nothing proxies for a
// machine that has no cluster PKI to be proxied with.
//
// --insecure goes after the subcommand: it is a flag on `version`, not a
// global, and talosctl rejects the invocation outright when it comes first.
func (r *Runner) inMaintenance(node string) bool {
	_, err := r.Output("--endpoints", node, "--nodes", node, "version", "--insecure")

	return err == nil
}

// EtcdState is what a control plane can tell talman about its etcd.
type EtcdState int

// The three answers, and the difference between the last two matters: the
// remedy for a cluster that was never bootstrapped is `talman apply --bootstrap`, and
// the remedy for a bootstrapped cluster that has lost quorum is anything but.
const (
	// EtcdUnknown is a node that did not answer, or answered in a shape
	// talman does not recognise. It is not evidence of anything.
	EtcdUnknown EtcdState = iota
	// EtcdStopped is etcd not running: no cluster has been bootstrapped here,
	// and a control plane sits in the booting stage waiting for one.
	EtcdStopped
	// EtcdRunning is etcd up, healthy or not. A degraded cluster is still a
	// cluster.
	EtcdRunning
)

// Etcd asks a control plane about its etcd service.
func (r *Runner) Etcd(talosconfig, node string) EtcdState {
	out, err := r.askNode(talosconfig, node, "get", "services", "etcd", "--output", "yaml")
	if err != nil {
		// Answered, and there is no etcd service: a control plane that has
		// just taken its config serves the Talos API before its services
		// are registered. No etcd service is no etcd running -- which is
		// what tells apply there is no cluster yet -- rather than a failure
		// to find out.
		var exit *ExitError
		if errors.As(err, &exit) && strings.Contains(exit.Stderr, "code = NotFound") {
			return EtcdStopped
		}

		return EtcdUnknown
	}

	return parseServiceRunning(out)
}

// parseServiceRunning finds a service resource's running flag.
//
// Walked structurally like the other resource readers here: the layout is
// Talos', and a release that moves the fields should read as "cannot tell"
// rather than as a wrong answer. A block is only taken for the service's
// status if it carries both flags, and running anywhere in the document wins
// -- a nested per-instance block saying no must not outvote the one saying
// yes.
func parseServiceRunning(out []byte) EtcdState {
	dec := yaml.NewDecoder(bytes.NewReader(out))

	state := EtcdUnknown

	for {
		var doc any

		if err := dec.Decode(&doc); err != nil {
			return state
		}

		switch found := running(doc); found {
		case EtcdRunning:
			return EtcdRunning
		case EtcdStopped:
			state = EtcdStopped
		case EtcdUnknown:
		}
	}
}

func running(node any) EtcdState {
	state := EtcdUnknown

	switch v := node.(type) {
	case map[string]any:
		up, hasRunning := v["running"].(bool)
		_, hasHealthy := v["healthy"].(bool)

		if hasRunning && hasHealthy {
			if up {
				return EtcdRunning
			}

			state = EtcdStopped
		}

		for _, child := range v {
			if found := running(child); found > state {
				state = found
			}

			if state == EtcdRunning {
				return EtcdRunning
			}
		}
	case []any:
		for _, child := range v {
			if found := running(child); found > state {
				state = found
			}

			if state == EtcdRunning {
				return EtcdRunning
			}
		}
	}

	return state
}

// Reachable reports whether the node answers the Talos API.
//
// Asked of the node first and through the endpoints second, so it answers
// while the control planes are down -- a cluster being built -- and also when
// the node's own API is not reachable from here, which is how workers are
// commonly firewalled. See askNode.
func (r *Runner) Reachable(talosconfig, node string) bool {
	_, err := r.askNode(talosconfig, node, "version")

	return err == nil
}

// WaitReady blocks until a node has answered the API continuously for the
// stabilization window, or until timeout.
//
// Continuously, not once: a node that has applied a config reboots, and a
// single successful probe can land in the window before it goes down. Holding
// the check for a settling period is what makes "the node came back" mean it.
//
// It says what it is waiting for while it waits. A node adopted out of
// maintenance mode installs Talos to disk and reboots, which takes minutes,
// and it is away for all of them -- so the version that only spoke when a node
// first answered printed one line and then nothing, for up to the whole
// timeout. Silence and a hang look identical from a terminal.
func (r *Runner) WaitReady(talosconfig, node string, stabilize, timeout time.Duration, log func(string, ...any)) error {
	const (
		poll = 5 * time.Second
		// Often enough to show the wait is alive, rarely enough not to bury
		// the output of a roll-out in ticks.
		report = 30 * time.Second
	)

	started := time.Now()
	deadline := started.Add(timeout)

	log("  waiting for %s to come back, up to %s", node, timeout)

	var (
		steadySince time.Time
		lastReport  time.Time
	)

	for {
		if r.Reachable(talosconfig, node) {
			if steadySince.IsZero() {
				steadySince = time.Now()

				log("  %s responding after %s; holding %s to confirm it stays up",
					node, round(time.Since(started)), stabilize)
			}

			if time.Since(steadySince) >= stabilize {
				return nil
			}
		} else {
			if !steadySince.IsZero() {
				steadySince = time.Time{}

				log("  %s went away again; restarting the stabilization window", node)
			}

			if time.Since(lastReport) >= report {
				lastReport = time.Now()

				log("  %s %s (%s elapsed)", node, r.awayBecause(node), round(time.Since(started)))
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not stay reachable for %s within %s "+
				"(--timeout waits longer; a node installing Talos for the first time pulls an "+
				"installer image before it can reboot)", node, stabilize, timeout)
		}

		if err := interrupt.Sleep(poll); err != nil {
			return fmt.Errorf("stopped waiting for %s: %w", node, err)
		}
	}
}

// awayBecause distinguishes a node that has not rebooted yet from one that is
// down, which is the difference between an install still running and an
// install that failed.
func (r *Runner) awayBecause(node string) string {
	if r.inMaintenance(node) {
		return "still in maintenance mode, installing"
	}

	return "not answering yet, probably rebooting"
}

// round trims a duration to something worth reading in a progress line.
func round(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second)
	}

	return d.Round(10 * time.Second)
}

// MachineConfig is the machine config a node is running, as Talos serves it:
// the MachineConfig resource, whose spec is the config document itself.
//
// Asked for rather than read from disk. The file under /system/state is where
// a docker node keeps it, but a machine installed to disk has no such path in
// its API's view, and "read" fails there with "no such file or directory".
func (r *Runner) MachineConfig(talosconfig, node string) ([]byte, error) {
	out, err := r.Output(append(r.NodeArgs(talosconfig, node),
		"get", "machineconfig", "v1alpha1", "--output", "yaml")...)
	if err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(out))

	for {
		var doc struct {
			Spec string `yaml:"spec"`
		}

		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("talosctl get machineconfig on %s returned no machine config", node)
		}

		if strings.TrimSpace(doc.Spec) != "" {
			return []byte(doc.Spec), nil
		}
	}
}
