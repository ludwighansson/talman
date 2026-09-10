package factory

import "testing"

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
			cfg:  Config{SecureBoot: true},
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
