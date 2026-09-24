package factory

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSchematicIDs pins the schematic ID algorithm.
//
// factory.Schematic is a local re-declaration of the upstream image-factory
// type, kept out of the module so talman has no Talos dependency. The ID is
// the sha256 of the marshalled struct, so a reordered or retagged field
// silently changes every installer image URL. These vectors were taken from
// the upstream implementation and must not be updated to match a code change.
func TestSchematicIDs(t *testing.T) {
	tests := []struct {
		name      string
		schematic Schematic
		want      string
	}{
		{
			name:      "vanilla",
			schematic: Schematic{},
			want:      "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba",
		},
		{
			name: "system extensions",
			schematic: Schematic{Customization: Customization{
				SystemExtensions: SystemExtensions{OfficialExtensions: []string{
					"siderolabs/drbd", "siderolabs/gvisor", "siderolabs/iscsi-tools",
				}},
			}},
			want: "079113ce0508c2b803971ea6cce43fc95c10eff57a123699b5c76fdc773132ae",
		},
		{
			name: "kernel args and META",
			schematic: Schematic{Customization: Customization{
				ExtraKernelArgs: []string{"vga=791", "net.ifnames=0"},
				Meta:            []MetaValue{{Key: 10, Value: "{}"}},
			}},
			want: "c7d733c8a404203329b512d19b2f5cfa18b364e5d832fc78dd15767c1feffd4c",
		},
		{
			name: "overlay, secureboot and disk image",
			schematic: Schematic{
				Overlay: Overlay{
					Image:   "ghcr.io/siderolabs/sbc-raspberry-pi",
					Name:    "rpi_generic",
					Options: map[string]any{"data": "mydata"},
				},
				Customization: Customization{
					Bootloader: "sd-boot",
					SecureBoot: SecureBoot{EnrollKeys: "force", IncludeWellKnownCertificates: true},
					DiskImage:  DiskImage{SectorSize: 4096},
				},
			},
			want: "f0fabc39847e1df488bbab08935429ad35118c7eee08668e67c26d39b04e85a9",
		},
		{
			// YAML 1.1 boolean lookalikes. The emitter has to quote these to
			// keep them strings, and *how* it quotes them is part of the
			// hashed bytes -- so this pins talman's YAML library against the
			// one image-factory marshals with. Both currently resolve to
			// go.yaml.in/yaml/v4; if a bump makes them disagree, the IDs
			// talman computes stop matching the factory's and these fail.
			name: "overlay option that looks like a boolean",
			schematic: Schematic{Overlay: Overlay{
				Image:   "i",
				Name:    "n",
				Options: map[string]any{"enable": "yes"},
			}},
			want: "5bb4bb4964217d3d94cd9083dc7369030f95c197725a603fe404431d96bd6702",
		},
		{
			name: "META value that looks like a boolean",
			schematic: Schematic{Customization: Customization{
				Meta: []MetaValue{{Key: 10, Value: "off"}},
			}},
			want: "ea8ac3e47398bf729c9977565b54caa17ddbf921b07c7d7108010cbf4219c63b",
		},
		{
			name: "kernel args that look like a boolean and a number",
			schematic: Schematic{Customization: Customization{
				ExtraKernelArgs: []string{"on", "1.20"},
			}},
			want: "ed2a7df88618bd3b7f06463479c625c40189664f98f19cca2aaf169caddd47fd",
		},
		{
			name: "embedded machine configuration",
			schematic: Schematic{Customization: Customization{
				EmbeddedMachineConfiguration: "apiVersion: v1alpha1\nkind: HostnameConfig\nhostname: x\n",
			}},
			want: "9ac3dc5caac49e82fbbec38c43da76931796494729505840e038aea736ac46f1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.schematic.ID()
			if err != nil {
				t.Fatalf("ID(): %v", err)
			}

			if got != tt.want {
				t.Errorf("ID() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestVanillaIDConstant(t *testing.T) {
	got, err := (&Schematic{}).ID()
	if err != nil {
		t.Fatal(err)
	}

	if got != VanillaID {
		t.Errorf("VanillaID = %s, but the empty schematic hashes to %s", VanillaID, got)
	}
}

func TestUnmarshalRejectsUnknownFields(t *testing.T) {
	// A typo one level above an extension list would otherwise produce a
	// different, silently valid schematic ID.
	_, err := Unmarshal([]byte("customization:\n  systemExtension:\n    officialExtensions: [a]\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown field, got nil")
	}
}

func TestInstallerURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "default is the v1.14 platform-scoped path",
			cfg:  Config{},
			want: "factory.talos.dev/metal-installer/abc:v1.14.0",
		},
		{
			name: "secureboot inserts the suffix",
			cfg:  Config{SecureBoot: new(true)},
			want: "factory.talos.dev/metal-installer-secureboot/abc:v1.14.0",
		},
		{
			name: "platform is substituted",
			cfg:  Config{Platform: "openstack"},
			want: "factory.talos.dev/openstack-installer/abc:v1.14.0",
		},
		{
			name: "a custom template wins",
			cfg: Config{
				RegistryURL:      "harbor.example.net",
				InstallerURLTmpl: "{{.RegistryURL}}/openstack-installer/{{.ID}}:{{.Version}}",
			},
			want: "harbor.example.net/openstack-installer/abc:v1.14.0",
		},
		{
			name: "the talhelper .Mode spelling still resolves",
			cfg:  Config{Platform: "metal", InstallerURLTmpl: "{{.RegistryURL}}/{{.Mode}}-installer/{{.ID}}:{{.Version}}"},
			want: "factory.talos.dev/metal-installer/abc:v1.14.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.InstallerURL("abc", "v1.14.0")
			if err != nil {
				t.Fatalf("InstallerURL(): %v", err)
			}

			if got != tt.want {
				t.Errorf("InstallerURL() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestInstallerURLReportsBadTemplate(t *testing.T) {
	_, err := Config{InstallerURLTmpl: "{{.Nope"}.InstallerURL("abc", "v1.14.0")
	if err == nil {
		t.Fatal("expected an error for a malformed template, got nil")
	}
}

// TestUpstreamParity pins IDs computed by image-factory v1.7.0's own
// pkg/schematic (Unmarshal, then ID) for the same input. talman re-declares
// the schematic types rather than importing them, so this is the only thing
// that notices when the two disagree -- which is how `bootloader`, an enum
// upstream and a plain string here, once produced IDs the factory never
// would.
func TestUpstreamParity(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"empty", "customization: {}\n", VanillaID},
		{"bootloader none is the zero value", "customization:\n  bootloader: none\n", VanillaID},
		{"sd-boot", "customization:\n  bootloader: sd-boot\n",
			"9ed5fecdacb36b5c5427b87d409f1065cfb2df69b0f71c58b868d9d466d8dab3"},
		{"names are case-insensitive", "customization:\n  bootloader: SD-BOOT\n",
			"9ed5fecdacb36b5c5427b87d409f1065cfb2df69b0f71c58b868d9d466d8dab3"},
		{"grub", "customization:\n  bootloader: grub\n",
			"39d496b2cbdb6265d3b714514c5334bf010f1b4d31b23b9e38c80fb2f3ad7ecb"},
		{"dual-boot", "customization:\n  bootloader: dual-boot\n",
			"43a1a6104d8dcd6547983f4ed13abb6f5e8a1b2fdad796c69e7db6e95d122884"},
		{"every field", `overlay:
  image: siderolabs/sbc-raspberrypi
  name: rpi_generic
  options:
    configTxtAppend: dtoverlay=disable-bt
customization:
  extraKernelArgs: [console=ttyS0, net.ifnames=0]
  meta:
    - key: 12
      value: '{"a":1}'
  systemExtensions:
    officialExtensions: [siderolabs/iscsi-tools, siderolabs/drbd]
  bootloader: grub
  secureboot:
    enrollKeys: force
    includeWellKnownCertificates: true
  diskImage:
    sectorSize: 4096
`, "95fda781b9dfb0c0dda9d34bade4de002c3f64665d3e73674a2b1768babd2ef9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Unmarshal([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}

			got, err := s.ID()
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("ID() = %s, upstream computes %s", got, tt.want)
			}
		})
	}

	if _, err := Unmarshal([]byte("customization:\n  bootloader: bogus\n")); err == nil {
		t.Error("an unknown bootloader was accepted; upstream rejects it")
	}
}

func TestSubmit(t *testing.T) {
	var gotBody, gotType, gotMethod, gotPath string

	status, reply := http.StatusCreated, `{"id":"abc123"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody, gotType, gotMethod, gotPath = string(body), r.Header.Get("Content-Type"), r.Method, r.URL.Path

		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	defer srv.Close()

	cfg := Config{
		RegistryURL: strings.TrimPrefix(srv.URL, "http://"),
		Protocol:    "http",
	}

	s := &Schematic{Customization: Customization{
		SystemExtensions: SystemExtensions{OfficialExtensions: []string{"siderolabs/drbd"}},
	}}

	id, err := cfg.Submit(s)
	if err != nil {
		t.Fatal(err)
	}

	canonical, _ := s.Marshal()

	switch {
	case id != "abc123":
		t.Errorf("id = %q, want the factory's", id)
	case gotMethod != http.MethodPost || gotPath != DefaultSchematicEndpoint:
		t.Errorf("request was %s %s, want POST %s", gotMethod, gotPath, DefaultSchematicEndpoint)
	case gotType != "application/yaml":
		t.Errorf("Content-Type = %q", gotType)
	case gotBody != string(canonical):
		t.Errorf("body = %q, want the canonical form %q", gotBody, canonical)
	}

	for _, tt := range []struct {
		name   string
		status int
		reply  string
		want   string
	}{
		{"refused", http.StatusBadRequest, "invalid schematic", "invalid schematic"},
		{"no id", http.StatusOK, `{}`, "contained no id"},
		{"not json", http.StatusOK, `<html>`, "decoding schematic response"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, reply = tt.status, tt.reply

			if _, err := cfg.Submit(s); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Submit() error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestImageURL pins the boot media URLs to the shapes the public factory
// serves; each was checked against factory.talos.dev.
func TestImageURL(t *testing.T) {
	const id, v = "abc", "v1.14.0"

	tests := []struct {
		name         string
		cfg          Config
		kind, format string
		arch         string
		want         string
	}{
		{"iso", Config{}, KindISO, "", "amd64", "https://factory.talos.dev/image/abc/v1.14.0/metal-amd64.iso"},
		{"secure boot iso", Config{SecureBoot: new(true)}, KindISO, "", "amd64",
			"https://factory.talos.dev/image/abc/v1.14.0/metal-amd64-secureboot.iso"},
		{"metal disk", Config{}, KindDisk, "", "arm64", "https://factory.talos.dev/image/abc/v1.14.0/metal-arm64.raw.zst"},
		{"platform disk", Config{Platform: "vmware"}, KindDisk, "", "amd64",
			"https://factory.talos.dev/image/abc/v1.14.0/vmware-amd64.ova"},
		{"explicit format", Config{}, KindDisk, "qcow2", "amd64",
			"https://factory.talos.dev/image/abc/v1.14.0/metal-amd64.qcow2"},
		{"pxe", Config{}, KindPXE, "", "amd64", "https://factory.talos.dev/pxe/abc/v1.14.0/metal-amd64"},
		{"own factory", Config{RegistryURL: "factory.internal", Protocol: "http"}, KindISO, "", "amd64",
			"http://factory.internal/image/abc/v1.14.0/metal-amd64.iso"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cfg.ImageURL(tt.kind, id, v, tt.arch, tt.format)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("ImageURL() = %s, want %s", got, tt.want)
			}
		})
	}

	if _, err := (Config{Platform: "somewhere-new"}).ImageURL(KindDisk, id, v, "amd64", ""); err == nil {
		t.Error("a platform with no known disk format was given one anyway")
	}
}
