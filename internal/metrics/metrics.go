// Package metrics records what a talman run did, node by node, in the
// Prometheus text format, for a CI job to alert on.
//
// A run is written to a file, pushed to a metrics push endpoint, or both. Both
// carry the same text: the file is for whatever collects files -- a textfile
// collector, a CI artefact -- and the push saves every pipeline from having to
// get the grouping of its pushes right.
package metrics

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Node results. Every node a run planned to touch ends up in exactly one.
const (
	ResultChanged    = "changed"     // it changed, or would have in a dry run
	ResultUnchanged  = "unchanged"   // nothing to do
	ResultDone       = "done"        // it succeeded; whether it changed is not known
	ResultFailed     = "failed"      // it failed
	ResultNotReached = "not_reached" // the run stopped before it got there
)

var results = []string{ResultChanged, ResultUnchanged, ResultDone, ResultFailed, ResultNotReached}

// Reserved are the label names talman sets itself, which an extra label may
// not reuse.
var Reserved = []string{"job", "cluster", "command", "dry_run", "node", "role", "result",
	"from_version", "to_version", "talman_version"}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// ParseLabel reads a key=value extra label.
func ParseLabel(s string) (string, string, error) {
	key, value, ok := strings.Cut(s, "=")
	if !ok {
		return "", "", fmt.Errorf("metrics label %q is not key=value", s)
	}

	key = strings.TrimSpace(key)

	if !labelName.MatchString(key) || strings.HasPrefix(key, "__") {
		return "", "", fmt.Errorf("metrics label %q: %q is not a valid Prometheus label name", s, key)
	}

	if slices.Contains(Reserved, key) {
		return "", "", fmt.Errorf("metrics label %q: talman sets %q itself", s, key)
	}

	if value == "" {
		return "", "", fmt.Errorf("metrics label %q has no value", s)
	}

	return key, value, nil
}

// Run is one command's run. A nil *Run records nothing, so a command can
// record unconditionally and pay nothing when metrics are off.
type Run struct {
	Command string
	Version string

	mu       sync.Mutex
	cluster  string
	labels   map[string]string
	start    time.Time
	end      time.Time
	success  bool
	changed  *bool
	order    []string
	nodes    map[string]*node
	finished bool
}

type node struct {
	role     string
	started  time.Time
	duration time.Duration
	changed  *bool
	err      error
	done     bool
	info     map[string]string
}

// NewRun starts recording a run.
func NewRun(command, version string, labels map[string]string) *Run {
	l := make(map[string]string, len(labels))
	for k, v := range labels {
		l[k] = v
	}

	return &Run{
		Command: command,
		Version: version,
		labels:  l,
		start:   time.Now(),
		nodes:   map[string]*node{},
	}
}

// SetCluster names the cluster, once the config that names it has been read.
func (r *Run) SetCluster(name string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.cluster = name
}

// SetLabel adds a label talman sets itself, such as dry_run.
func (r *Run) SetLabel(key, value string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.labels[key] = value
}

// Plan names the nodes the run means to touch, in order. A node planned and
// never started is reported as not reached.
func (r *Run) Plan(hostname, role string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.node(hostname, role)
}

// NodeStart marks the moment talman began on a node.
func (r *Run) NodeStart(hostname, role string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.node(hostname, role).started = time.Now()
}

// NodeChanged records whether a node changed, when talman knows.
func (r *Run) NodeChanged(hostname string, changed bool) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.node(hostname, "").changed = &changed
}

// NodeInfo records a descriptive label for a node, such as the version it was
// upgraded from.
func (r *Run) NodeInfo(hostname, key, value string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	n := r.node(hostname, "")
	if n.info == nil {
		n.info = map[string]string{}
	}

	n.info[key] = value
}

// NodeDone records how a node's part of the run ended.
func (r *Run) NodeDone(hostname string, err error) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	n := r.node(hostname, "")
	n.done = true
	n.err = err

	if !n.started.IsZero() {
		n.duration = time.Since(n.started)
	}
}

// SetChanged records whether the run as a whole changed anything, for runs
// whose answer is not the sum of their nodes'.
func (r *Run) SetChanged(changed bool) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.changed = &changed
}

// Finish ends the run.
func (r *Run) Finish(success bool) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.end = time.Now()
	r.success = success
	r.finished = true
}

// Cluster is the cluster the run was against, "unknown" when its config could
// not be read.
func (r *Run) Cluster() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.clusterLocked()
}

func (r *Run) clusterLocked() string {
	if r.cluster == "" {
		return "unknown"
	}

	return r.cluster
}

// Grouping is the set of labels that identifies this run among others -- the
// cluster, the command, and the extra labels -- in a stable order.
func (r *Run) Grouping() [][2]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.groupingLocked()
}

func (r *Run) groupingLocked() [][2]string {
	keys := make([]string, 0, len(r.labels))
	for k := range r.labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	out := make([][2]string, 0, 2+len(keys))
	out = append(out, [2]string{"cluster", r.clusterLocked()}, [2]string{"command", r.Command})

	for _, k := range keys {
		out = append(out, [2]string{k, r.labels[k]})
	}

	return out
}

func (r *Run) node(hostname, role string) *node {
	n, ok := r.nodes[hostname]
	if !ok {
		n = &node{}
		r.nodes[hostname] = n
		r.order = append(r.order, hostname)
	}

	if role != "" {
		n.role = role
	}

	return n
}

func (n *node) result() string {
	switch {
	case !n.done:
		return ResultNotReached
	case n.err != nil:
		return ResultFailed
	case n.changed == nil:
		return ResultDone
	case *n.changed:
		return ResultChanged
	default:
		return ResultUnchanged
	}
}

// changedLocked is the run's answer to "did anything change": what the command
// said, or else what its nodes said, and unknown when any of them could not
// tell.
func (r *Run) changedLocked() *bool {
	if r.changed != nil {
		return r.changed
	}

	if len(r.order) == 0 {
		return nil
	}

	unknown := false

	for _, h := range r.order {
		switch r.nodes[h].result() {
		case ResultChanged:
			changed := true

			return &changed
		case ResultUnchanged:
		default:
			unknown = true
		}
	}

	if unknown {
		return nil
	}

	changed := false

	return &changed
}

// Text renders the run in the Prometheus text exposition format.
func (r *Run) Text() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	end := r.end
	if end.IsZero() {
		end = time.Now()
	}

	base := r.groupingLocked()

	var b bytes.Buffer

	family := func(name, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
	}

	sample := func(name string, labels [][2]string, value float64) {
		b.WriteString(name)
		b.WriteString(formatLabels(labels))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
		b.WriteByte('\n')
	}

	with := func(extra ...[2]string) [][2]string {
		return append(slices.Clone(base), extra...)
	}

	family("talman_run_success", "Whether the run succeeded (1) or failed (0).")
	sample("talman_run_success", base, boolValue(r.success))

	if changed := r.changedLocked(); changed != nil {
		family("talman_run_changed", "Whether the run changed anything, or would have in a dry run.")
		sample("talman_run_changed", base, boolValue(*changed))
	}

	family("talman_run_duration_seconds", "How long the run took.")
	sample("talman_run_duration_seconds", base, seconds(end.Sub(r.start)))

	family("talman_run_timestamp_seconds", "When the run ended, as a Unix timestamp.")
	sample("talman_run_timestamp_seconds", base, float64(end.Unix()))

	if r.success {
		family("talman_run_last_success_timestamp_seconds", "When a run last succeeded, as a Unix timestamp.")
		sample("talman_run_last_success_timestamp_seconds", base, float64(end.Unix()))
	}

	family("talman_run_info", "The talman that made the run.")
	sample("talman_run_info", with([2]string{"talman_version", r.Version}), 1)

	if len(r.order) == 0 {
		return b.Bytes()
	}

	counts := map[string]int{}
	for _, h := range r.order {
		counts[r.nodes[h].result()]++
	}

	family("talman_run_nodes", "How many of the run's nodes ended in each result.")

	for _, res := range results {
		sample("talman_run_nodes", with([2]string{"result", res}), float64(counts[res]))
	}

	nodeLabels := func(h string) [][2]string {
		return with([2]string{"node", h}, [2]string{"role", r.nodes[h].role})
	}

	family("talman_node_result", "The result each node ended in; one series per node, always 1.")

	for _, h := range r.order {
		sample("talman_node_result", append(nodeLabels(h), [2]string{"result", r.nodes[h].result()}), 1)
	}

	family("talman_node_success", "Whether talman's work on a node succeeded (1) or failed (0), for nodes it reached.")

	for _, h := range r.order {
		if n := r.nodes[h]; n.done {
			sample("talman_node_success", nodeLabels(h), boolValue(n.err == nil))
		}
	}

	family("talman_node_duration_seconds", "How long talman spent on a node, for nodes it reached.")

	for _, h := range r.order {
		if n := r.nodes[h]; n.done && !n.started.IsZero() {
			sample("talman_node_duration_seconds", nodeLabels(h), seconds(n.duration))
		}
	}

	var withInfo []string

	for _, h := range r.order {
		if len(r.nodes[h].info) > 0 {
			withInfo = append(withInfo, h)
		}
	}

	if len(withInfo) > 0 {
		family("talman_node_info", "Descriptive labels for a node, such as the versions an upgrade moved it between.")

		for _, h := range withInfo {
			info := r.nodes[h].info

			keys := make([]string, 0, len(info))
			for k := range info {
				keys = append(keys, k)
			}

			sort.Strings(keys)

			labels := nodeLabels(h)
			for _, k := range keys {
				labels = append(labels, [2]string{k, info[k]})
			}

			sample("talman_node_info", labels, 1)
		}
	}

	return b.Bytes()
}

// WriteFile writes the run to path, replacing whatever was there in one step so
// a collector never reads half a file.
//
// A failed run carries forward the last-success timestamp of the run it
// replaces, when it is the same run -- same cluster, command and labels.
// Otherwise the one metric that says how long it has been since this job last
// worked would vanish exactly when it starts to matter.
func (r *Run) WriteFile(path string) error {
	text := r.Text()

	r.mu.Lock()
	success := r.success
	grouping := r.groupingLocked()
	r.mu.Unlock()

	if !success {
		if previous, ok := lastSuccess(path, grouping); ok {
			text = append(text, []byte(fmt.Sprintf(
				"# HELP talman_run_last_success_timestamp_seconds When a run last succeeded, as a Unix timestamp.\n"+
					"# TYPE talman_run_last_success_timestamp_seconds gauge\n"+
					"talman_run_last_success_timestamp_seconds%s %s\n",
				formatLabels(grouping), previous))...)
		}
	}

	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename; best effort otherwise

	if _, err := tmp.Write(text); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the one that matters

		return err
	}

	// Readable by whatever collects it: this is a report, not a secret.
	if err := tmp.Chmod(0o644); err != nil { //nolint:gosec // see above
		tmp.Close() //nolint:errcheck // the chmod error is the one that matters

		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// lastSuccess finds the last-success timestamp for the same grouping in a
// file written by an earlier run.
func lastSuccess(path string, grouping [][2]string) (string, bool) {
	f, err := os.Open(path) //nolint:gosec // the operator's own metrics file
	if err != nil {
		return "", false
	}
	defer f.Close() //nolint:errcheck // read-only

	want := "talman_run_last_success_timestamp_seconds" + formatLabels(grouping) + " "

	s := bufio.NewScanner(f)
	for s.Scan() {
		if value, ok := strings.CutPrefix(s.Text(), want); ok {
			return strings.TrimSpace(value), true
		}
	}

	return "", false
}

func formatLabels(labels [][2]string) string {
	if len(labels) == 0 {
		return ""
	}

	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l[0]+`="`+escape(l[1])+`"`)
	}

	return "{" + strings.Join(parts, ",") + "}"
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(v string) string {
	return escaper.Replace(v)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

func seconds(d time.Duration) float64 {
	return float64(d.Milliseconds()) / 1000
}
