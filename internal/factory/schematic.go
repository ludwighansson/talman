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
	"strings"
	"text/template"
	"time"

	"go.yaml.in/yaml/v4"
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
	Bootloader                   string           `yaml:"bootloader,omitempty"`
	SecureBoot                   SecureBoot       `yaml:"secureboot,omitempty"`
	DiskImage                    DiskImage        `yaml:"diskImage,omitempty"`
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
	SecureBoot        bool   `yaml:"secureboot,omitempty"`
}

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
		SecureBoot:  c.SecureBoot,
		Secureboot:  c.SecureBoot,
	}

	var buf bytes.Buffer

	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering imageFactory.installerURLTmpl: %w", err)
	}

	return buf.String(), nil
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

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
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
