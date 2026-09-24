package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateSchema = flag.Bool("update-schema", false, "rewrite schema/talman.v1.json from the config types")

// TestJSONSchemaUpToDate keeps the published schema in step with the types:
// run `go test ./internal/config -update-schema` after changing them.
func TestJSONSchemaUpToDate(t *testing.T) {
	want, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join("..", "..", "schema", "talman.v1.json")

	if *updateSchema {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/config -update-schema)", err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("%s is out of date with the config types: run go test ./internal/config -update-schema", path)
	}
}
