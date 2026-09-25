// Package redact hides a cluster's own secrets in output that would otherwise
// carry them.
package redact

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// Marker is what a secret is replaced with. Visible, so a reader can tell the
// difference between a value that was hidden and a value that was empty.
const Marker = "[redacted]"

// minSecret is the shortest value worth treating as one when nothing says it
// is a secret except where it was found.
//
// Everything a Talos secrets bundle holds is far longer -- the shortest is a
// join token at 23 characters -- and a threshold keeps a short scalar that
// happens to sit in the bundle from blanking unrelated words wherever they
// appear in a diff.
const minSecret = 16

// minDeclared is the shortest value hidden when the operator said it was a
// secret by encrypting it with SOPS. A registry password can be short, and
// the operator's word is better evidence than its length.
const minDeclared = 6

// minPEMLine is the shortest line of a PEM body worth hiding: the final line
// of a key is often short, and it is still part of the key.
const minPEMLine = 8

// Set collects the secret values a pass comes across. Safe for concurrent use,
// because a render pass renders nodes concurrently.
type Set struct {
	mu   sync.Mutex
	seen map[string]bool
}

// NewSet returns an empty set.
func NewSet() *Set {
	return &Set{seen: map[string]bool{}}
}

// AddBundle adds every value in a decrypted Talos secrets bundle.
//
// By value, not by field name. talman knows what this cluster's secrets are,
// because it decrypts them and renders them into the machine configs it sends,
// so it can find them wherever they appear rather than guessing which keys in
// Talos' YAML are sensitive. A guess that misses one line would be worse than
// no redaction at all: it implies a safety it does not provide.
//
// The bundle holds its keys as base64, and a machine config carries some of
// them that way and some decoded -- the Kubernetes CA and service-account keys
// are PEM blocks in their own documents. Both forms are added, the decoded one
// line by line: a diff prints each line of a block with its own prefix and
// indentation, so a match on the whole block would never land.
func (s *Set) AddBundle(plaintext []byte) error {
	var doc any

	if err := yaml.Unmarshal(plaintext, &doc); err != nil {
		return err
	}

	before := s.Len()

	walk(doc, func(v string) {
		s.addLong(v, minSecret)
		s.addDecoded(v)
	})

	if s.Len() == before {
		return errors.New("no secrets found in the bundle")
	}

	return nil
}

// AddEncrypted adds the values SOPS encrypted in a file, given the file as it
// is on disk and as it decrypts.
//
// Only the values that were encrypted: a patch encrypted with an
// encrypted_regex holds plain registry hostnames beside the password, and
// those are not secrets. Everything, if it was encrypted whole.
//
// Every document, side by side: sops writes its metadata into each document of
// a multi-document file, and a patch's second document -- a RegistryAuthConfig
// after the node's own -- holds its own secrets.
func (s *Set) AddEncrypted(ciphertext, plaintext []byte) error {
	encDocs, err := documents(ciphertext)
	if err != nil {
		return err
	}

	decDocs, err := documents(plaintext)
	if err != nil {
		return err
	}

	for i := range min(len(encDocs), len(decDocs)) {
		pair(encDocs[i], decDocs[i], func(v string) { s.addLong(v, minDeclared) })
	}

	return nil
}

// documents decodes every YAML document in data.
func documents(data []byte) ([]any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))

	var out []any

	for {
		var doc any

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}

		if err != nil {
			return nil, err
		}

		out = append(out, doc)
	}
}

// Add adds values that came from somewhere a secret plausibly does, such as
// the environment a template read. They count from minDeclared, as values an
// operator encrypted do: the environment is where a CI job keeps what it would
// not commit, and a registry password there is often short.
func (s *Set) Add(values ...string) {
	for _, v := range values {
		s.addLong(v, minDeclared)
	}
}

// Len is how many distinct secrets the set holds.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.seen)
}

// Redactor freezes what the set knows into something that can replace it.
func (s *Set) Redactor() *Redactor {
	s.mu.Lock()
	defer s.mu.Unlock()

	values := make([]string, 0, len(s.seen))
	for v := range s.seen {
		values = append(values, v)
	}

	// Longest first, so a secret that contains another is replaced whole
	// rather than leaving a tail of the shorter one behind.
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}

		return values[i] < values[j]
	})

	// With their base64 forms: a secret written into an inline manifest's
	// Secret, or anywhere a template piped it through b64enc, appears only
	// encoded. The encodings are no shorter than the value, so the order
	// above still holds for them.
	var encoded []string

	// Count is of secrets; their encodings are only more ways to find them.
	count := len(values)

	for _, v := range values {
		for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
			if e := enc.EncodeToString([]byte(v)); !s.seen[e] {
				encoded = append(encoded, e)
			}
		}
	}

	values = append(values, encoded...)
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}

		return values[i] < values[j]
	})

	values = slices.Compact(values)

	pairs := make([]string, 0, len(values)*2)
	for _, v := range values {
		pairs = append(pairs, v, Marker)
	}

	return &Redactor{replacer: strings.NewReplacer(pairs...), count: count}
}

func (s *Set) addLong(v string, least int) {
	v = strings.TrimSpace(v)
	if len(v) < least {
		return
	}

	s.mu.Lock()
	s.seen[v] = true
	s.mu.Unlock()
}

// addDecoded adds the text a base64 value decodes to, when it decodes to text:
// the lines of a PEM block, individually, and any other text whole.
func (s *Set) addDecoded(v string) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || len(raw) == 0 || !utf8.Valid(raw) {
		return
	}

	text := string(raw)

	if !strings.Contains(text, "-----BEGIN ") {
		if printable(text) {
			s.addLong(text, minSecret)
		}

		return
	}

	for _, line := range strings.Split(text, "\n") {
		// The armour lines are the same in every PEM block, and hiding them
		// would blank every certificate header in the diff for nothing.
		if strings.HasPrefix(strings.TrimSpace(line), "-----") {
			continue
		}

		s.addLong(line, minPEMLine)
	}
}

func printable(text string) bool {
	for _, r := range text {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}

	return true
}

// Redactor replaces known secret values wherever they appear.
type Redactor struct {
	replacer *strings.Replacer
	count    int
}

// FromBundle builds a redactor from a decrypted Talos secrets bundle alone.
//
// It follows that only this cluster's secrets are found. A diff against a node
// holding some other cluster's keys shows those, because talman has never seen
// them.
func FromBundle(plaintext []byte) (*Redactor, error) {
	s := NewSet()

	if err := s.AddBundle(plaintext); err != nil {
		return nil, err
	}

	return s.Redactor(), nil
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

// walk calls fn for every string leaf.
func walk(node any, fn func(string)) {
	switch v := node.(type) {
	case string:
		fn(v)
	case map[string]any:
		for _, child := range v {
			walk(child, fn)
		}
	case []any:
		for _, child := range v {
			walk(child, fn)
		}
	}
}

// pair walks a SOPS file and its decryption side by side, calling fn with the
// plaintext of every leaf the file holds encrypted.
func pair(enc, dec any, fn func(string)) {
	switch e := enc.(type) {
	case string:
		if !strings.HasPrefix(e, "ENC[") {
			return
		}

		// Whatever type sops recorded: a port or a PIN encrypted as an int
		// is as much a secret as a string.
		switch d := dec.(type) {
		case string:
			fn(d)
		case int, int64, uint64, float64, bool:
			fn(fmt.Sprint(d))
		}
	case map[string]any:
		d, _ := dec.(map[string]any)

		for k, child := range e {
			if k == "sops" {
				continue
			}

			pair(child, d[k], fn)
		}
	case []any:
		d, _ := dec.([]any)

		for i, child := range e {
			if i < len(d) {
				pair(child, d[i], fn)
			}
		}
	}
}
