package template

import (
	"strings"
	"testing"
)

func ctx() Context {
	return Context{
		Cluster: Cluster{
			Name:              "development",
			Endpoint:          "https://10.0.0.1:6443",
			TalosVersion:      "v1.14.0",
			KubernetesVersion: "v1.37.0",
		},
		Node: Node{
			Hostname:       "w1",
			IPAddress:      "10.0.0.10",
			Role:           "worker",
			Groups:         []string{"db", "storage"},
			Values:         map[string]any{"installDisk": "/dev/vda", "zone": "az1"},
			TalosVersion:   "v1.14.0",
			SchematicID:    "abc",
			InstallerImage: "factory.talos.dev/metal-installer/abc:v1.14.0",
		},
		Values: map[string]any{"harbor": "harbor.example.net"},
	}
}

func TestRenderScopes(t *testing.T) {
	tests := []struct{ name, tmpl, want string }{
		{"cluster", "{{ .Cluster.Name }}", "development"},
		{"node", "{{ .Node.Hostname }}/{{ .Node.Role }}", "w1/worker"},
		{"node values", `{{ .Node.Values.installDisk }}`, "/dev/vda"},
		{"values", "{{ .Values.harbor }}", "harbor.example.net"},
		{"installer image", "{{ .Node.InstallerImage }}", "factory.talos.dev/metal-installer/abc:v1.14.0"},
		{"schematic id", "{{ .Node.SchematicID }}", "abc"},
		{"has group method", `{{ if .Node.HasGroup "db" }}yes{{ else }}no{{ end }}`, "yes"},
		{"has group negative", `{{ if .Node.HasGroup "gpu" }}yes{{ else }}no{{ end }}`, "no"},
		{"sprig has on groups", `{{ if has "storage" .Node.Groups }}yes{{ end }}`, "yes"},
		{"sprig semverCompare", `{{ if semverCompare ">=1.14.0-0" .Node.TalosVersion }}new{{ end }}`, "new"},
		{"sprig default", `{{ .Node.Values.zone | default "unknown" }}`, "az1"},
		{"range over values", `{{ range $k, $v := .Values }}{{ $k }}={{ $v }}{{ end }}`, "harbor=harbor.example.net"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Render("t.yaml", []byte(tt.tmpl), ctx())
			if err != nil {
				t.Fatalf("Render(): %v", err)
			}

			if string(got) != tt.want {
				t.Errorf("Render(%q) = %q, want %q", tt.tmpl, got, tt.want)
			}
		})
	}
}

// TestMissingKeyIsAnError is the whole reason missingkey=error is set: without
// it, a typo in .Node.Values renders the literal "<no value>" into a machine
// config and Talos accepts a subtly wrong value.
func TestMissingKeyIsAnError(t *testing.T) {
	for _, tmpl := range []string{
		"{{ .Node.Values.instalDisk }}",
		"{{ .Values.habror }}",
	} {
		if _, err := Render("t.yaml", []byte(tmpl), ctx()); err == nil {
			t.Errorf("Render(%q) succeeded; want an error", tmpl)
		}
	}
}

func TestErrorsNameTheFileOnce(t *testing.T) {
	_, err := Render("patches/all/x.yaml", []byte("{{ .Nope }}"), ctx())
	if err == nil {
		t.Fatal("expected an error")
	}

	if got := strings.Count(err.Error(), "patches/all/x.yaml"); got != 1 {
		t.Errorf("file named %d times in %q, want once", got, err)
	}
}

func TestParseErrorIsReported(t *testing.T) {
	if _, err := Render("t.yaml", []byte("{{ if }}"), ctx()); err == nil {
		t.Fatal("expected a parse error")
	}
}

// A patch wrapped entirely in a conditional is a normal idiom; the renderer
// must be able to produce nothing at all.
func TestConditionalDocumentCanRenderEmpty(t *testing.T) {
	got, err := Render("t.yaml", []byte(`{{ if .Node.HasGroup "gpu" }}kind: X{{ end }}`), ctx())
	if err != nil {
		t.Fatal(err)
	}

	if strings.TrimSpace(string(got)) != "" {
		t.Errorf("got %q, want empty", got)
	}
}
