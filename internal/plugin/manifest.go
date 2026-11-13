package plugin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const (
	ManifestSchema   = 1
	MaxManifestBytes = 256 << 10
)

var (
	IDPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	KeyPattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	SlotIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	MetricPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,95}$`)
	FileNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Activation says when a plugin process should run.
type Activation string

const (
	ActivationAlways  Activation = "always"
	ActivationOnSlot  Activation = "on-slot"
	ActivationOnEvent Activation = "on-event"
)

// Role mirrors the web UI roles.
type Role string

const (
	RoleViewer    Role = "viewer"
	RoleResponder Role = "responder"
	RoleAdmin     Role = "admin"
)

// RoleRank orders roles: viewer 1, responder 2, admin 3, anything else 0.
func RoleRank(r Role) int {
	switch r {
	case RoleViewer:
		return 1
	case RoleResponder:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Manifest is plugin.json.
type Manifest struct {
	Schema       int           `json:"schema"`
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	Publisher    string        `json:"publisher"`
	Description  string        `json:"description"`
	Homepage     string        `json:"homepage,omitempty"`
	HostAPI      string        `json:"host_api"`
	MinTrinetra  string        `json:"min_trinetra,omitempty"`
	Permissions  []Permission  `json:"permissions"`
	Binaries     []Binary      `json:"binaries"`
	ConfigSchema *ConfigSchema `json:"config_schema,omitempty"`
	Contributes  Contributes   `json:"contributes"`
	Activation   Activation    `json:"activation,omitempty"`
}

// Binary is one per-platform executable in the package.
type Binary struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Contributes lists the UI and files a plugin adds.
type Contributes struct {
	Slots   []SlotDecl   `json:"slots,omitempty"`
	Actions []ActionDecl `json:"actions,omitempty"`
	Files   []FileDecl   `json:"files,omitempty"`
}

// SlotDecl declares one slot the plugin fills.
type SlotDecl struct {
	Slot    Slot   `json:"slot"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	MinRole Role   `json:"min_role,omitempty"`
}

// ActionDecl declares one invocable action; role and permission are enforced from here, never from a widget.
type ActionDecl struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	Confirm    string     `json:"confirm,omitempty"`
	MinRole    Role       `json:"min_role"`
	Permission Permission `json:"permission,omitempty"`
	Mutating   bool       `json:"mutating"`
}

// FileDecl declares a downloadable file shipped at files/<Name> in the package.
type FileDecl struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	ContentType string `json:"content_type"`
}

// DecodeManifest strictly decodes plugin.json (unknown fields and trailing bytes rejected) and validates it.
func DecodeManifest(b []byte) (Manifest, error) {
	var m Manifest
	if len(b) > MaxManifestBytes {
		return m, fmt.Errorf("manifest: larger than %d bytes", MaxManifestBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("manifest: trailing bytes after the JSON document")
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func runesIn(s string, lo, hi int) bool {
	n := utf8.RuneCountInString(s)
	return n >= lo && n <= hi
}

// Validate checks every field rule.
func (m Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		return fmt.Errorf("manifest: schema must be %d", ManifestSchema)
	}
	if !IDPattern.MatchString(m.ID) {
		return fmt.Errorf("manifest: id %q is invalid", m.ID)
	}
	if !runesIn(m.Name, 1, 64) {
		return errors.New("manifest: name must be 1-64 characters")
	}
	if _, err := update.ParseVersion(m.Version); err != nil {
		return fmt.Errorf("manifest: version: %w", err)
	}
	if !runesIn(m.Publisher, 1, 64) {
		return errors.New("manifest: publisher must be 1-64 characters")
	}
	if !runesIn(m.Description, 0, 280) {
		return errors.New("manifest: description must be at most 280 characters")
	}
	if m.Homepage != "" && !strings.HasPrefix(m.Homepage, "https://") {
		return errors.New("manifest: homepage must be an https:// URL")
	}
	if _, _, err := ParseHostAPI(m.HostAPI); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if m.MinTrinetra != "" {
		if _, err := update.ParseVersion(m.MinTrinetra); err != nil {
			return fmt.Errorf("manifest: min_trinetra: %w", err)
		}
	}
	seenPerm := map[Permission]bool{}
	for _, p := range m.Permissions {
		if p.Reserved() {
			return fmt.Errorf("manifest: permissions: permission %q is reserved for a later release", p)
		}
		if !p.Known() {
			return fmt.Errorf("manifest: permissions: unknown permission %q", p)
		}
		if seenPerm[p] {
			return fmt.Errorf("manifest: permissions: duplicate %q", p)
		}
		seenPerm[p] = true
	}
	if err := m.validateBinaries(); err != nil {
		return err
	}
	switch m.Activation {
	case "", ActivationAlways, ActivationOnSlot, ActivationOnEvent:
	default:
		return fmt.Errorf("manifest: activation %q is invalid", m.Activation)
	}
	if m.ConfigSchema != nil {
		if err := m.ConfigSchema.Validate(); err != nil {
			return fmt.Errorf("manifest: config_schema: %w", err)
		}
	}
	return m.validateContributes()
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (m Manifest) validateBinaries() error {
	if len(m.Binaries) == 0 {
		return errors.New("manifest: binaries: at least one binary is required")
	}
	seen := map[string]bool{}
	for _, b := range m.Binaries {
		if b.OS != "linux" && b.OS != "darwin" {
			return fmt.Errorf("manifest: binaries: os %q is not supported", b.OS)
		}
		if b.Arch != "amd64" && b.Arch != "arm64" && b.Arch != "arm" {
			return fmt.Errorf("manifest: binaries: arch %q is not supported", b.Arch)
		}
		if !sha256Hex.MatchString(b.SHA256) {
			return fmt.Errorf("manifest: binaries: sha256 for %s/%s must be 64 lowercase hex characters", b.OS, b.Arch)
		}
		if b.Size <= 0 {
			return fmt.Errorf("manifest: binaries: size for %s/%s must be positive", b.OS, b.Arch)
		}
		k := b.OS + "/" + b.Arch
		if seen[k] {
			return fmt.Errorf("manifest: binaries: duplicate %s", k)
		}
		seen[k] = true
	}
	return nil
}

func validRole(r Role) bool { return RoleRank(r) > 0 }

func (m Manifest) validateContributes() error {
	actions := map[string]bool{}
	for _, a := range m.Contributes.Actions {
		if !SlotIDPattern.MatchString(a.ID) {
			return fmt.Errorf("manifest: action id %q is invalid", a.ID)
		}
		if actions[a.ID] {
			return fmt.Errorf("manifest: action %q is declared twice", a.ID)
		}
		actions[a.ID] = true
		if !runesIn(a.Label, 1, 40) {
			return fmt.Errorf("manifest: action %q label must be 1-40 characters", a.ID)
		}
		if !validRole(a.MinRole) {
			return fmt.Errorf("manifest: action %q min_role is invalid", a.ID)
		}
		if a.Permission != "" && !a.Permission.Known() {
			return fmt.Errorf("manifest: action %q permission %q is unknown", a.ID, a.Permission)
		}
	}
	seen := map[string]bool{}
	for _, s := range m.Contributes.Slots {
		known := false
		for _, v := range V1Slots {
			known = known || v == s.Slot
		}
		if !known {
			return fmt.Errorf("manifest: slot %q is unknown", s.Slot)
		}
		if !SlotIDPattern.MatchString(s.ID) {
			return fmt.Errorf("manifest: slot id %q is invalid", s.ID)
		}
		k := string(s.Slot) + "/" + s.ID
		if seen[k] {
			return fmt.Errorf("manifest: slot %s is declared twice", k)
		}
		seen[k] = true
		if !runesIn(s.Title, 1, 64) {
			return fmt.Errorf("manifest: slot %s title must be 1-64 characters", k)
		}
		if s.MinRole != "" && !validRole(s.MinRole) {
			return fmt.Errorf("manifest: slot %s min_role is invalid", k)
		}
		if s.Slot.HostRendered() && !m.hasSection(s.Slot) {
			return fmt.Errorf("manifest: slot %s needs config_schema properties in that section", k)
		}
		if s.Slot == SlotContainerAction && !actions[s.ID] {
			return fmt.Errorf("manifest: slot %s needs an action with the same id", k)
		}
	}
	files := map[string]bool{}
	for _, f := range m.Contributes.Files {
		if !FileNamePattern.MatchString(f.Name) {
			return fmt.Errorf("manifest: file name %q is invalid", f.Name)
		}
		if files[f.Name] {
			return fmt.Errorf("manifest: file %q is declared twice", f.Name)
		}
		files[f.Name] = true
		if f.ContentType != "application/json" && f.ContentType != "text/plain" {
			return fmt.Errorf("manifest: file %q content_type %q is not allowed", f.Name, f.ContentType)
		}
	}
	return nil
}

func (m Manifest) hasSection(s Slot) bool {
	if m.ConfigSchema == nil {
		return false
	}
	for _, p := range m.ConfigSchema.Properties {
		if p.SectionOf() == s {
			return true
		}
	}
	return false
}

// BinaryFor returns the binary entry for goos/goarch.
func (m Manifest) BinaryFor(goos, goarch string) (Binary, bool) {
	for _, b := range m.Binaries {
		if b.OS == goos && b.Arch == goarch {
			return b, true
		}
	}
	return Binary{}, false
}

// SlotDecl returns the declaration for slot/id.
func (m Manifest) SlotDecl(slot Slot, id string) (SlotDecl, bool) {
	for _, s := range m.Contributes.Slots {
		if s.Slot == slot && s.ID == id {
			return s, true
		}
	}
	return SlotDecl{}, false
}

// ActionDecl returns the declaration for action id.
func (m Manifest) ActionDecl(id string) (ActionDecl, bool) {
	for _, a := range m.Contributes.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return ActionDecl{}, false
}

// BinaryName is "trinetra-plugin-<id>".
func BinaryName(id string) string { return "trinetra-plugin-" + id }

// BinaryPath is "bin/<os>-<arch>/trinetra-plugin-<id>".
func BinaryPath(goos, goarch, id string) string {
	return "bin/" + goos + "-" + goarch + "/" + BinaryName(id)
}
