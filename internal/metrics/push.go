package metrics

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Job is the job label a push is grouped under.
const Job = "talman"

// PushURL is where a run is pushed: the endpoint's base URL, followed by the
// job and the run's grouping labels as path segments, which is how a metrics
// push endpoint tells one group of metrics from another.
//
// A value that cannot be a path segment as it is -- it holds a slash, or is
// empty -- is written in URL-safe base64 with the label name marked @base64,
// as the protocol allows.
func (r *Run) PushURL(base string) (string, error) {
	u, err := parseURL(base)
	if err != nil {
		return "", err
	}

	// Built escaped and unescaped side by side: a base64 value may hold '='
	// and a label value anything, and the URL has to carry both forms for
	// the escaping to survive.
	var raw, plain strings.Builder

	raw.WriteString(strings.TrimRight(u.EscapedPath(), "/") + "/metrics/job/" + Job)
	plain.WriteString(strings.TrimRight(u.Path, "/") + "/metrics/job/" + Job)

	for _, l := range r.Grouping() {
		name, value := l[0], l[1]

		if value == "" || strings.Contains(value, "/") {
			name += "@base64"
			value = base64.RawURLEncoding.EncodeToString([]byte(value))

			if value == "" {
				value = "="
			}
		}

		raw.WriteString("/" + url.PathEscape(name) + "/" + url.PathEscape(value))
		plain.WriteString("/" + name + "/" + value)
	}

	u.Path = plain.String()
	u.RawPath = raw.String()

	return u.String(), nil
}

// CheckURL reports whether base is an endpoint a run could be pushed to, so a
// mistyped one is refused before the run, as a mistyped label is, rather than
// warned about once the run is over and its metrics are lost.
func CheckURL(base string) error {
	_, err := parseURL(base)

	return err
}

func parseURL(base string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("metrics URL %q: %w", base, err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("metrics URL %q must be http or https", base)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("metrics URL %q names no host", base)
	}

	return u, nil
}

// Push sends the run to a metrics push endpoint.
//
// POST rather than PUT: a POST replaces the metrics it names and leaves the
// rest of the group alone. A failed run does not send a last-success
// timestamp, so the one from the last run that worked stays where it is --
// which is what an alert on "how long since this job last worked" needs.
// Every other metric is sent on every run, node series included, so a node
// that has left the cluster does not linger.
func (r *Run) Push(ctx context.Context, base, token string) error {
	target, err := r.PushURL(base)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(r.Text()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushing metrics: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read below, nothing to do on failure

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))

		return fmt.Errorf("pushing metrics: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	return nil
}
