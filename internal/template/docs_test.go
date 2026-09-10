package template

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// mangledAction matches a single-braced template action, e.g. `{ .Node.Values.x }`.
//
// That is never something anyone writes on purpose: it is what a careless
// YAML reformat leaves behind after eating one layer of braces from
// `{{ .Node.Values.x }}`. The result is still valid YAML -- a flow mapping
// with a null value -- so it renders, validates and ships silently. Only a
// reader notices, which is exactly how it got into the README once.
var mangledAction = regexp.MustCompile(`(^|[^{])\{\s*\.[A-Za-z_][\w.]*\s*\}([^}]|$)`)

func TestNoMangledTemplateActions(t *testing.T) {
	root := repoRoot(t)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "clusterconfig" || name == "dist" {
				return filepath.SkipDir
			}

			return nil
		}

		switch filepath.Ext(path) {
		case ".md", ".yaml", ".yml":
		default:
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for i, line := range strings.Split(string(body), "\n") {
			if mangledAction.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d: single-braced template action, should be {{ ... }}:\n  %s",
					rel, i+1, strings.TrimSpace(line))
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDocumentedTemplateScopesExist keeps the README's table of template
// scopes honest: every .Node.X and .Cluster.X it advertises must be a real
// field, so a renamed field cannot leave the docs pointing at nothing.
func TestDocumentedTemplateScopesExist(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "README.md"))
	if err != nil {
		t.Fatal(err)
	}

	ctx := Context{
		Cluster: Cluster{Name: "c"},
		Node:    Node{Hostname: "h", Values: map[string]any{"k": "v"}},
		Values:  map[string]any{"k": "v"},
	}

	refs := regexp.MustCompile(`\.(?:Node|Cluster)\.[A-Z][A-Za-z]*`).FindAllString(string(body), -1)
	if len(refs) == 0 {
		t.Fatal("found no .Node./.Cluster. references in the README; has it moved?")
	}

	seen := map[string]bool{}

	for _, ref := range refs {
		if seen[ref] || strings.HasPrefix(ref, ".Node.Values") {
			continue
		}

		seen[ref] = true

		_, err := Render("README.md", []byte("{{ "+ref+" }}"), ctx)
		if err == nil {
			continue
		}

		// A method such as .Node.HasGroup exists but needs an argument;
		// complaining about the arity proves the symbol is there, which is
		// all this test is asserting.
		if strings.Contains(err.Error(), "wrong number of args") {
			continue
		}

		t.Errorf("README documents %s, which does not resolve: %v", ref, err)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	// internal/template -> repo root
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}
