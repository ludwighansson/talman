package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestJSONShapes pins the -o json output scripts read: a key renamed, or an
// empty list turned null, breaks them, and fails here first.
func TestJSONShapes(t *testing.T) {
	dir, _ := exitFixtureWith(t, `  - hostname: w1
    ipAddress: 10.0.0.2
    role: worker
    groups: [blue]
    patches: [./w1.yaml]
`)

	if err := os.WriteFile(filepath.Join(dir, "w1.yaml"), []byte("machine: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const vanilla = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"patches", "-o", "json"}, `{
  "nodes": [
    {
      "hostname": "c1",
      "role": "controlplane",
      "groups": [],
      "patches": []
    },
    {
      "hostname": "w1",
      "role": "worker",
      "groups": [
        "blue"
      ],
      "patches": [
        {
          "group": "node",
          "path": "./w1.yaml"
        }
      ]
    }
  ]
}
`},
		{[]string{"schematic", "id", "-o", "json", "-n", "c1"}, `{
  "nodes": [
    {
      "hostname": "c1",
      "schematicID": "` + vanilla + `"
    }
  ]
}
`},
		{[]string{"image", "url", "-o", "json", "-n", "c1"}, `{
  "nodes": [
    {
      "hostname": "c1",
      "installerImage": "factory.talos.dev/metal-installer/` + vanilla + `:v1.14.1"
    }
  ]
}
`},
		{[]string{"image", "url", "--kind", "iso", "-o", "json", "-n", "c1"}, `{
  "nodes": [
    {
      "hostname": "c1",
      "iso": "https://factory.talos.dev/image/` + vanilla + `/v1.14.1/metal-amd64.iso"
    }
  ]
}
`},
	} {
		var got int

		out := captureStdout(t, func() { got = run(tt.args) })

		if got != 0 {
			t.Errorf("%v: exit %d", tt.args, got)

			continue
		}

		if out != tt.want {
			t.Errorf("%v printed\n%s\nwant\n%s", tt.args, out, tt.want)
		}
	}
}
