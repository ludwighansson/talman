// Package factory models Image Factory schematics and installer image URLs.
//
// The Schematic type is a deliberate local re-declaration of
// github.com/siderolabs/image-factory/pkg/schematic. Importing that package
// would pull github.com/siderolabs/talos/pkg/machinery into the module for the
// sake of one enum, and talman's whole premise is that it never depends on the
// Talos API. Field order and yaml tags are copied exactly, because the
// schematic ID is the sha256 of the marshalled struct -- reordering a field
// would silently change every ID.
//
// Verified byte-identical to upstream for the vanilla, system-extension,
// kernel-arg/META, overlay/secureboot/disk-image and embedded-config cases.
package factory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/ludwighansson/talman/internal/interrupt"
)

// Defaults for the public Image Factory.
const (
	DefaultRegistryURL       = "factory.talos.dev"
	DefaultProtocol          = "https"
	DefaultSchematicEndpoint = "/schematics"

	// DefaultInstallerURLTmpl matches what `talosctl gen config` defaults to on
	// Talos v1.14: the platform-scoped installer path, not the legacy
	// factory.talos.dev/installer/<id> form talhelper emitted.
	DefaultInstallerURLTmpl = "{{.RegistryURL}}/{{.Platform}}-installer{{if .SecureBoot}}-secureboot{{end}}/{{.ID}}:{{.Version}}"

	// VanillaID is the ID of the empty schematic, used as the fallback when no
	// schematic is configured.
	VanillaID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
)

// Schematic represents the requested image customization.
type Schematic struct {
	Owner         string        `yaml:"owner,omitempty"`
	Overlay       Overlay       `yaml:"overlay,omitempty"`
	Customization Customization `yaml:"customization"`
}

// Customization represents the Talos image customization.
type Customization struct {
	EmbeddedMachineConfiguration string           `yaml:"embeddedMachineConfiguration,omitempty"`
	ExtraKernelArgs              []string         `yaml:"extraKernelArgs,omitempty"`
	Meta                         []MetaValue      `yaml:"meta,omitempty"`
	SystemExtensions             SystemExtensions `yaml:"systemExtensions,omitempty"`
	Bootloader                   Bootloader       `yaml:"bootloader,omitempty"`
	SecureBoot                   SecureBoot       `yaml:"secureboot,omitempty"`
	DiskImage                    DiskImage        `yaml:"diskImage,omitempty"`
}

// Bootloader is the image's bootloader.
//
// Upstream this is an integer enum that marshals as its name, and its zero
// value is "none" -- so `bootloader: none` is omitted from the canonical form
// altogether, names are matched case-insensitively and written lower case, and
// anything else is rejected. A plain string got all three wrong, and each one
// is a different schematic ID from the one the factory computes.
type Bootloader string

// The bootloaders the Image Factory accepts; none is the zero value.
var bootloaders = []Bootloader{"none", "dual-boot", "sd-boot", "grub"}

// UnmarshalYAML normalises and checks the name.
func (b *Bootloader) UnmarshalYAML(value *yaml.Node) error {
	var s string

	if err := value.Decode(&s); err != nil {
		return err
	}

	name := Bootloader(strings.ToLower(s))

	if !slices.Contains(bootloaders, name) {
		return fmt.Errorf("line %d: bootloader %q is not one of none, dual-boot, sd-boot, grub", value.Line, s)
	}

	if name == "none" {
		name = ""
	}

	*b = name

	return nil
}

// MetaValue provides initial META contents for the image.
type MetaValue struct {
	Key   uint8  `yaml:"key"`
	Value string `yaml:"value"`
}

// SystemExtensions represents the Talos system extensions to be installed.
type SystemExtensions struct {
	OfficialExtensions []string `yaml:"officialExtensions,omitempty"`
}

// Overlay represents the overlay options for image generation.
type Overlay struct {
	Image   string         `yaml:"image"`
	Name    string         `yaml:"name"`
	Options map[string]any `yaml:"options,omitempty"`
}

// SecureBoot represents the secure boot options for the image.
type SecureBoot struct {
	EnrollKeys                   string `yaml:"enrollKeys,omitempty"`
	IncludeWellKnownCertificates bool   `yaml:"includeWellKnownCertificates,omitempty"`
}

// DiskImage represents the disk image options for the image.
type DiskImage struct {
	SectorSize uint `yaml:"sectorSize,omitempty"`
}

// Unmarshal parses a schematic, rejecting unknown fields so a typo in an
// extension name's parent key is an error rather than a silently different ID.
func Unmarshal(data []byte) (*Schematic, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var s Schematic

	if err := dec.Decode(&s); err != nil {
		return nil, err
	}

	return &s, nil
}

// Marshal renders the canonical representation whose hash is the ID.
func (s *Schematic) Marshal() ([]byte, error) {
	return yaml.Marshal(s)
}

// ID returns the sha256 of the canonical representation.
func (s *Schematic) ID() (string, error) {
	data, err := s.Marshal()
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// Config describes where images live and how their URLs are spelled.
type Config struct {
	RegistryURL       string `yaml:"registryURL,omitempty"`
	Protocol          string `yaml:"protocol,omitempty"`
	SchematicEndpoint string `yaml:"schematicEndpoint,omitempty"`
	InstallerURLTmpl  string `yaml:"installerURLTmpl,omitempty"`
	Platform          string `yaml:"platform,omitempty"`
	// SecureBoot is a pointer so a node's override can turn it off again.
	SecureBoot *bool `yaml:"secureBoot,omitempty"`
}

// Override returns c with every field o sets replacing c's.
func (c Config) Override(o *Config) Config {
	if o == nil {
		return c
	}

	for _, f := range []struct{ dst, src *string }{
		{&c.RegistryURL, &o.RegistryURL},
		{&c.Protocol, &o.Protocol},
		{&c.SchematicEndpoint, &o.SchematicEndpoint},
		{&c.InstallerURLTmpl, &o.InstallerURLTmpl},
		{&c.Platform, &o.Platform},
	} {
		if *f.src != "" {
			*f.dst = *f.src
		}
	}

	if o.SecureBoot != nil {
		c.SecureBoot = o.SecureBoot
	}

	return c
}

// SecureBootEnabled reports whether installer images are the secure boot ones.
func (c Config) SecureBootEnabled() bool { return c.SecureBoot != nil && *c.SecureBoot }

// WithDefaults returns a copy with every unset field filled in.
func (c Config) WithDefaults() Config {
	if c.RegistryURL == "" {
		c.RegistryURL = DefaultRegistryURL
	}

	if c.Protocol == "" {
		c.Protocol = DefaultProtocol
	}

	if c.SchematicEndpoint == "" {
		c.SchematicEndpoint = DefaultSchematicEndpoint
	}

	if c.InstallerURLTmpl == "" {
		c.InstallerURLTmpl = DefaultInstallerURLTmpl
	}

	if c.Platform == "" {
		c.Platform = "metal"
	}

	return c
}

// urlData is the template context for InstallerURLTmpl. The field names are
// part of the user-facing contract, so they are kept compatible with the
// talhelper spelling operators are likely to be migrating from.
type urlData struct {
	RegistryURL string
	Protocol    string
	ID          string
	Version     string
	Platform    string
	Mode        string // alias of Platform, for talhelper compatibility
	SecureBoot  bool
	Secureboot  bool // alias, ditto
}

// InstallerURL renders the installer image reference for a schematic ID and
// Talos version.
func (c Config) InstallerURL(schematicID, talosVersion string) (string, error) {
	c = c.WithDefaults()

	tmpl, err := template.New("installerURL").Parse(c.InstallerURLTmpl)
	if err != nil {
		return "", fmt.Errorf("parsing imageFactory.installerURLTmpl: %w", err)
	}

	data := urlData{
		RegistryURL: c.RegistryURL,
		Protocol:    c.Protocol,
		ID:          schematicID,
		Version:     talosVersion,
		Platform:    c.Platform,
		Mode:        c.Platform,
		SecureBoot:  c.SecureBootEnabled(),
		Secureboot:  c.SecureBootEnabled(),
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering imageFactory.installerURLTmpl: %w", err)
	}

	return buf.String(), nil
}

// Boot media kinds ImageURL can name, besides the installer image.
const (
	KindISO  = "iso"
	KindDisk = "disk"
	KindPXE  = "pxe"
)

// diskFormats is the disk image each platform is published as, by the file
// extension the factory serves it under. Only formats checked against the
// public factory are listed; any other platform has to be told.
var diskFormats = map[string]string{
	"metal":         "raw.zst",
	"aws":           "raw.xz",
	"azure":         "vhd.xz",
	"digital-ocean": "raw.gz",
	"exoscale":      "qcow2",
	"gcp":           "raw.tar.gz",
	"hcloud":        "raw.xz",
	"nocloud":       "raw.xz",
	"openstack":     "raw.xz",
	"scaleway":      "raw.zst",
	"upcloud":       "raw.xz",
	"vmware":        "ova",
}

// ImageURL is the factory URL of a boot medium for a schematic: an ISO, a
// disk image, or the iPXE script that netboots it. format overrides the disk
// image's extension, and is required for a platform talman has no default
// for.
func (c Config) ImageURL(kind, schematicID, talosVersion, arch, format string) (string, error) {
	c = c.WithDefaults()

	name := c.Platform + "-" + arch
	if c.SecureBootEnabled() {
		name += "-secureboot"
	}

	base := c.Protocol + "://" + c.RegistryURL

	switch kind {
	case KindISO:
		return fmt.Sprintf("%s/image/%s/%s/%s.iso", base, schematicID, talosVersion, name), nil
	case KindPXE:
		return fmt.Sprintf("%s/pxe/%s/%s/%s", base, schematicID, talosVersion, name), nil
	case KindDisk:
		if format == "" {
			format = diskFormats[c.Platform]
		}

		if format == "" {
			return "", fmt.Errorf("talman has no default disk image format for platform %q: "+
				"pass --format with the extension the factory serves it as (raw.xz, qcow2, …)", c.Platform)
		}

		return fmt.Sprintf("%s/image/%s/%s/%s.%s", base, schematicID, talosVersion, name, format), nil
	default:
		return "", fmt.Errorf("unknown image kind %q", kind)
	}
}

// Submit POSTs a schematic to the Image Factory and returns the authoritative
// ID it assigns. The factory may canonicalise fields we do not know about, so
// a mismatch against the locally computed ID is reported rather than hidden.
func (c Config) Submit(s *Schematic) (string, error) {
	c = c.WithDefaults()

	body, err := s.Marshal()
	if err != nil {
		return "", err
	}

	url := c.Protocol + "://" + c.RegistryURL + c.SchematicEndpoint

	req, err := http.NewRequestWithContext(interrupt.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/yaml")

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("submitting schematic to %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // closing a response body whose contents are already read

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("schematic submission to %s failed: %s: %s",
			url, resp.Status, strings.TrimSpace(string(respBody)))
	}

	var out struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("decoding schematic response from %s: %w", url, err)
	}

	if out.ID == "" {
		return "", fmt.Errorf("schematic response from %s contained no id", url)
	}

	return out.ID, nil
}
