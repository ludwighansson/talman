package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func upgradeRun() *Run {
	r := NewRun("upgrade", "1.0.0", map[string]string{"env": "prod"})
	r.SetCluster("sto-com-dev")

	r.Plan("cp1", "controlplane")
	r.Plan("w1", "worker")
	r.Plan("w2", "worker")

	r.NodeStart("cp1", "controlplane")
	r.NodeInfo("cp1", "from_version", "v1.14.0")
	r.NodeInfo("cp1", "to_version", "v1.14.1")
	r.NodeChanged("cp1", true)
	r.NodeDone("cp1", nil)

	r.NodeStart("w1", "worker")
	r.NodeDone("w1", errors.New("boom"))

	return r
}

// What a run did, node by node, including the node it never reached because
// the run stopped first.
func TestTextReportsEveryNode(t *testing.T) {
	r := upgradeRun()
	r.Finish(false)

	got := string(r.Text())

	base := `cluster="sto-com-dev",command="upgrade",env="prod"`

	for _, want := range []string{
		"# TYPE talman_run_success gauge",
		"talman_run_success{" + base + "} 0",
		"talman_run_changed{" + base + "} 1",
		`talman_run_nodes{` + base + `,result="changed"} 1`,
		`talman_run_nodes{` + base + `,result="failed"} 1`,
		`talman_run_nodes{` + base + `,result="not_reached"} 1`,
		`talman_run_nodes{` + base + `,result="unchanged"} 0`,
		`talman_node_result{` + base + `,node="cp1",role="controlplane",result="changed"} 1`,
		`talman_node_result{` + base + `,node="w1",role="worker",result="failed"} 1`,
		`talman_node_result{` + base + `,node="w2",role="worker",result="not_reached"} 1`,
		`talman_node_success{` + base + `,node="cp1",role="controlplane"} 1`,
		`talman_node_success{` + base + `,node="w1",role="worker"} 0`,
		`talman_node_info{` + base + `,node="cp1",role="controlplane",from_version="v1.14.0",to_version="v1.14.1"} 1`,
		`talman_run_info{` + base + `,talman_version="1.0.0"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n%s", want, got)
		}
	}

	// A node talman never reached has no success or duration to report.
	for _, unwanted := range []string{
		`talman_node_success{` + base + `,node="w2"`,
		`talman_node_duration_seconds{` + base + `,node="w2"`,
		"talman_run_last_success_timestamp_seconds",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("unexpected %s\n%s", unwanted, got)
		}
	}
}

// "Did anything change" is only answered when it is known: one node that
// changed is enough for yes, but no needs every node to have said so.
func TestChangedIsOnlyReportedWhenKnown(t *testing.T) {
	unknown := NewRun("apply", "dev", nil)
	unknown.Plan("w1", "worker")
	unknown.NodeStart("w1", "worker")
	unknown.NodeDone("w1", nil)
	unknown.Finish(true)

	if strings.Contains(string(unknown.Text()), "talman_run_changed") {
		t.Error("reported a change it could not know about")
	}

	quiet := NewRun("apply", "dev", nil)
	quiet.NodeChanged("w1", false)
	quiet.NodeDone("w1", nil)
	quiet.Finish(true)

	if !strings.Contains(string(quiet.Text()), `talman_run_changed{cluster="unknown",command="apply"} 0`) {
		t.Errorf("an unchanged run was not reported as one:\n%s", quiet.Text())
	}
}

// A failed run keeps the last success of the run it replaces in the file, so
// "how long since this last worked" survives the failure it is there for.
func TestWriteFileCarriesTheLastSuccessThroughAFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "talman.prom")

	ok := NewRun("apply", "dev", nil)
	ok.SetCluster("c")
	ok.Finish(true)

	if err := ok.WriteFile(path); err != nil {
		t.Fatal(err)
	}

	first, _ := os.ReadFile(path)
	line := lineWith(string(first), "talman_run_last_success_timestamp_seconds{")

	if line == "" {
		t.Fatalf("a successful run wrote no last success:\n%s", first)
	}

	failed := NewRun("apply", "dev", nil)
	failed.SetCluster("c")
	failed.Finish(false)

	if err := failed.WriteFile(path); err != nil {
		t.Fatal(err)
	}

	second, _ := os.ReadFile(path)

	if lineWith(string(second), "talman_run_last_success_timestamp_seconds{") != line {
		t.Errorf("the last success did not survive the failure:\n%s", second)
	}

	if !strings.Contains(string(second), `talman_run_success{cluster="c",command="apply"} 0`) {
		t.Errorf("the failure itself was not written:\n%s", second)
	}

	// A different job's file is not a different job's history.
	other := NewRun("apply", "dev", nil)
	other.SetCluster("another")
	other.Finish(false)

	if err := other.WriteFile(path); err != nil {
		t.Fatal(err)
	}

	third, _ := os.ReadFile(path)

	if strings.Contains(string(third), "talman_run_last_success_timestamp_seconds") {
		t.Errorf("carried another cluster's last success:\n%s", third)
	}
}

func lineWith(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}

	return ""
}

func TestParseLabel(t *testing.T) {
	if k, v, err := ParseLabel("env=prod"); err != nil || k != "env" || v != "prod" {
		t.Errorf("ParseLabel(env=prod) = %q %q %v", k, v, err)
	}

	if _, v, err := ParseLabel("pipeline=a=b"); err != nil || v != "a=b" {
		t.Errorf("a value may hold '=': %q %v", v, err)
	}

	for _, bad := range []string{"env", "env=", "1env=x", "cluster=x", "node=x", "__name__=x", "my-label=x"} {
		if _, _, err := ParseLabel(bad); err == nil {
			t.Errorf("ParseLabel(%q) accepted it", bad)
		}
	}
}

// Each run is its own group at the endpoint, so an apply's push never
// replaces an upgrade's; a value a path cannot hold is sent as base64.
func TestPushURL(t *testing.T) {
	r := NewRun("apply", "dev", map[string]string{"pipeline": "nightly/drift"})
	r.SetCluster("sto-com-dev")
	r.SetLabel("dry_run", "true")

	got, err := r.PushURL("https://metrics.example.com/base/")
	if err != nil {
		t.Fatal(err)
	}

	want := "https://metrics.example.com/base/metrics/job/talman/cluster/sto-com-dev/command/apply" +
		"/dry_run/true/pipeline@base64/bmlnaHRseS9kcmlmdA"
	if got != want {
		t.Errorf("PushURL =\n %s\nwant\n %s", got, want)
	}

	if _, err := r.PushURL("ftp://nope"); err == nil {
		t.Error("accepted a URL that is not http")
	}
}

func TestPush(t *testing.T) {
	var method, path, auth, body string

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		method, path, auth = req.Method, req.URL.EscapedPath(), req.Header.Get("Authorization")

		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}))
	defer srv.Close()

	r := upgradeRun()
	r.Finish(true)

	if err := r.Push(context.Background(), srv.URL, "s3cret"); err != nil {
		t.Fatal(err)
	}

	// POST, so the last success of an earlier run survives a failed one.
	if method != http.MethodPost {
		t.Errorf("method = %s, want POST", method)
	}

	if path != "/metrics/job/talman/cluster/sto-com-dev/command/upgrade/env/prod" {
		t.Errorf("path = %s", path)
	}

	if auth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", auth)
	}

	if !strings.Contains(body, "talman_run_success") {
		t.Errorf("body is not the run:\n%s", body)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusBadRequest)
	}))
	defer failing.Close()

	if err := r.Push(context.Background(), failing.URL, ""); err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("a refused push was not reported: %v", err)
	}
}

// A nil run is what "metrics off" is, and recording into it must cost nothing
// and break nothing.
func TestNilRunRecordsNothing(_ *testing.T) {
	var r *Run

	r.SetCluster("c")
	r.SetLabel("k", "v")
	r.Plan("n", "worker")
	r.NodeStart("n", "worker")
	r.NodeChanged("n", true)
	r.NodeInfo("n", "k", "v")
	r.NodeDone("n", nil)
	r.SetChanged(true)
	r.Finish(true)
}
