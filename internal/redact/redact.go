// Package redact hides a cluster's own secrets in output that would otherwise
// carry them.
package redact

import (
	"errors"
	"sort"
	"strings"

	"go.yaml.in/yaml/v4"
)

// Marker is what a secret is replaced with. Visible, so a reader can tell the
// difference between a value that was hidden and a value that was empty.
const Marker = "[redacted]"

// minSecret is the shortest value worth treating as one.
//
// Everything a Talos secrets bundle holds is far longer -- the shortest is a
// join token at 23 characters -- and a threshold keeps a short scalar that
// happens to sit in the bundle from blanking unrelated words wherever they
// appear in a diff.
const minSecret = 16

// Redactor replaces known secret values wherever they appear.
type Redactor struct {
	replacer *strings.Replacer
	count    int
}

// FromBundle builds a redactor from a decrypted Talos secrets bundle.
//
// By value, not by field name. talman knows what this cluster's secrets are,
// because it decrypts them and renders them into the machine configs it sends,
// so it can find them wherever they appear rather than guessing which keys in
// Talos' YAML are sensitive. A guess that misses one line would be worse than
// no redaction at all: it implies a safety it does not provide.
//
// It follows that only this cluster's secrets are found. A diff against a node
// holding some other cluster's keys shows those, because talman has never seen
// them.
func FromBundle(plaintext []byte) (*Redactor, error) {
	var doc any

	if err := yaml.Unmarshal(plaintext, &doc); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	collect(doc, seen)

	if len(seen) == 0 {
		return nil, errors.New("no secrets found in the bundle")
	}

	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}

	// Longest first, so a secret that contains another is replaced whole
	// rather than leaving a tail of the shorter one behind.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })

	pairs := make([]string, 0, len(values)*2)
	for _, v := range values {
		pairs = append(pairs, v, Marker)
	}

	return &Redactor{replacer: strings.NewReplacer(pairs...), count: len(values)}, nil
}

// Count is how many distinct secrets the redactor knows.
func (r *Redactor) Count() int {
	if r == nil {
		return 0
	}

	return r.count
}

// String returns s with every known secret replaced.
func (r *Redactor) String(s string) string {
	if r == nil {
		return s
	}

	return r.replacer.Replace(s)
}

// Bytes is String for output captured from a command.
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil {
		return b
	}

	return []byte(r.replacer.Replace(string(b)))
}

func collect(node any, into map[string]bool) {
	switch v := node.(type) {
	case string:
		if len(strings.TrimSpace(v)) >= minSecret {
			into[v] = true
		}
	case map[string]any:
		for _, child := range v {
			collect(child, into)
		}
	case []any:
		for _, child := range v {
			collect(child, into)
		}
	}
}
