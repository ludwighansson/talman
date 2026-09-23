// Package template renders patch files as Go templates.
//
// Every referenced patch file goes through here -- there is no opt-in suffix.
// topf gates templating on a .tpl extension, which means a templated patch can
// never also be SOPS-encrypted; treating every patch as a template removes
// that wall at the cost of requiring literal braces to be escaped.
package template

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

// Cluster is the .Cluster template scope.
type Cluster struct {
	Name              string
	Endpoint          string
	TalosVersion      string
	KubernetesVersion string
}

// Node is the .Node template scope.
type Node struct {
	Hostname  string
	IPAddress string
	Role      string
	Groups    []string
	Values    map[string]any

	// TalosVersion is the node's effective version, already resolved against
	// the cluster default. topf exposes three similarly named fields with
	// different meanings here; talman exposes one, and it is the resolved one.
	TalosVersion string
	// SchematicID is the resolved Image Factory schematic ID.
	SchematicID string
	// InstallerImage is the full installer image reference for this node.
	InstallerImage string
}

// Context is the root template scope.
type Context struct {
	Cluster Cluster
	Node    Node
	Values  map[string]any
}

// HasGroup reports whether the node belongs to a group. Equivalent to sprig's
// `has`, but reads better in a patch: {{ if .Node.HasGroup "db" }}.
func (n Node) HasGroup(name string) bool {
	return slices.Contains(n.Groups, name)
}

// Render executes content as a template against ctx.
//
// missingkey=error is deliberate: .Values and .Node.Values are maps, so without
// it a typo renders the string "<no value>" into a machine config and Talos
// gets a subtly wrong value instead of an error.
func Render(name string, content []byte, ctx Context) ([]byte, error) {
	return RenderSeeing(name, content, ctx, nil)
}

// RenderSeeing is Render, telling seen about every value the template read from
// the environment.
//
// The environment is where a CI job keeps what it would not commit, so a value
// a patch pulled out of it is, for all talman can tell, a secret it has just
// rendered into a machine config -- and one that nothing else would let talman
// find again when it prints that config.
func RenderSeeing(name string, content []byte, ctx Context, seen func(string)) ([]byte, error) {
	funcs := sprig.TxtFuncMap()

	if seen != nil {
		funcs["env"] = func(key string) string {
			v := os.Getenv(key)
			seen(v)

			return v
		}

		funcs["expandenv"] = func(s string) string {
			return os.Expand(s, func(key string) string {
				v := os.Getenv(key)
				seen(v)

				return v
			})
		}
	}

	tmpl, err := template.New(name).
		Funcs(funcs).
		Option("missingkey=error").
		Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("%s: parsing template: %w", name, cleanErr(err, name))
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, ctx); err != nil {
		return nil, fmt.Errorf("%s: rendering template: %w", name, cleanErr(err, name))
	}

	return buf.Bytes(), nil
}

// cleanErr trims text/template's own bookkeeping out of an error so the
// message reads as one sentence about one file.
//
// text/template names the template twice -- once as a "template: <name>:"
// prefix and again as `executing "<name>" at` -- and refers to the context by
// its Go type, none of which means anything to someone editing a patch.
func cleanErr(err error, name string) error {
	msg := err.Error()

	msg = strings.TrimPrefix(msg, "template: "+name+": ")
	msg = strings.TrimPrefix(msg, "template: "+name+":")
	msg = executingRE.ReplaceAllString(msg, "")
	msg = strings.ReplaceAll(msg, "in type template.Context", "in the patch template context")

	return fmt.Errorf("%s", msg)
}

// executingRE matches text/template's `executing "<name>" at ` interjection.
var executingRE = regexp.MustCompile(`executing "[^"]*" at `)
