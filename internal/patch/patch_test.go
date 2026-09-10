package patch

import (
	"strings"
	"testing"
)

func TestAcceptsStrategicMerge(t *testing.T) {
	for _, name := range []string{"v1alpha1 fragment", "multi-doc", "patch delete"} {
		t.Run(name, func(t *testing.T) {
			var body string

			switch name {
			case "v1alpha1 fragment":
				body = "machine:\n  env:\n    FOO: bar\n"
			case "multi-doc":
				body = "apiVersion: v1alpha1\nkind: HostnameConfig\nhostname: a\n" +
					"---\napiVersion: v1alpha1\nkind: SysctlConfig\nparams: {a: \"1\"}\n"
			case "patch delete":
				body = "apiVersion: v1alpha1\nkind: KubeFlannelCNIConfig\n$patch: delete\n"
			}

			if err := CheckStrategicMerge("t.yaml", []byte(body)); err != nil {
				t.Errorf("rejected valid patch: %v", err)
			}
		})
	}
}

// Talos hard-rejects JSON6902 against multi-document configs, which is every
// config from v1.12 on. Its own error names no file, so talman pre-empts it.
func TestRejectsJSON6902(t *testing.T) {
	for _, body := range []string{
		`[{"op":"remove","path":"/machine/certSANs"}]`,
		"- op: add\n  path: /machine/env/FOO\n  value: bar\n",
		"- OP: replace\n  path: /x\n  value: 1\n",
	} {
		err := CheckStrategicMerge("bad.yaml", []byte(body))
		if err == nil {
			t.Errorf("accepted a JSON6902 patch: %s", body)

			continue
		}

		if !strings.Contains(err.Error(), "strategic merge") {
			t.Errorf("error does not suggest the fix: %v", err)
		}

		if !strings.Contains(err.Error(), "bad.yaml") {
			t.Errorf("error does not name the file: %v", err)
		}
	}
}

// A plain YAML list is not a JSON patch; only a list of op/path maps is.
func TestSequenceWithoutOpIsFine(t *testing.T) {
	body := "apiVersion: v1alpha1\nkind: ResolverConfig\nservers:\n  - 1.1.1.1\n  - 8.8.8.8\n"
	if err := CheckStrategicMerge("t.yaml", []byte(body)); err != nil {
		t.Errorf("rejected a normal list: %v", err)
	}
}

func TestReportsInvalidYAML(t *testing.T) {
	err := CheckStrategicMerge("t.yaml", []byte("machine:\n  - a\n bad indent: x\n"))
	if err == nil {
		t.Fatal("expected an error for malformed YAML")
	}

	if !strings.Contains(err.Error(), "t.yaml") {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestEmptyIsFine(t *testing.T) {
	for _, body := range []string{"", "\n\n", "# just a comment\n"} {
		if err := CheckStrategicMerge("t.yaml", []byte(body)); err != nil {
			t.Errorf("rejected empty content %q: %v", body, err)
		}
	}
}
