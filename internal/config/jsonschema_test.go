package config

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// The rule and description tables are keyed "Type.field" by hand, so a field
// renamed without its key would lose its rule silently. Every key has to name
// a yaml field of a type the config holds.
func TestSchemaTablesNameRealFields(t *testing.T) {
	fields := map[string]bool{}
	seen := map[reflect.Type]bool{}

	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
			t = t.Elem()
		}

		if t.Kind() != reflect.Struct || seen[t] {
			return
		}

		seen[t] = true

		for f := range t.Fields() {
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}

			// Untagged fields are a type's own decoding (SchematicRef's
			// path-or-inline), still walked for the types they hold.
			if name != "" {
				fields[t.Name()+"."+name] = true
			}

			walk(f.Type)
		}
	}

	walk(reflect.TypeFor[Config]())

	var keys []string
	for k := range schemaDescriptions {
		keys = append(keys, k)
	}

	for k := range schemaRules {
		keys = append(keys, k)
	}

	for k := range schemaReservedItems {
		keys = append(keys, k)
	}

	for k := range schemaNullable {
		keys = append(keys, k)
	}

	for k := range schemaMinItems {
		keys = append(keys, k)
	}

	for _, k := range keys {
		if !fields[k] {
			t.Errorf("schema table key %q names no yaml field", k)
		}
	}
}
