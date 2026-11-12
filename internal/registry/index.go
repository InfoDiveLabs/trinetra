package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

const (
	IndexSchema      = 1
	MaxIndexBytes    = 4 << 20
	MaxIndexLifetime = 30 * 24 * time.Hour
	clockSkew        = time.Hour
)

// Index is the signed registry catalog (index.json).
type Index struct {
	Schema  int                 `json:"schema"`
	Serial  int64               `json:"serial"`
	Issued  string              `json:"issued"`
	Expires string              `json:"expires"`
	Plugins []IndexPlugin       `json:"plugins"`
	Revoked []plugin.Revocation `json:"revoked,omitempty"`
}

// IndexPlugin is one plugin listed in the index.
type IndexPlugin struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Publisher   string         `json:"publisher"`
	Description string         `json:"description"`
	Homepage    string         `json:"homepage,omitempty"`
	Versions    []IndexVersion `json:"versions"`
}

// IndexVersion is one published version of a plugin.
type IndexVersion struct {
	Version     string              `json:"version"`
	HostAPI     string              `json:"host_api"`
	MinTrinetra string              `json:"min_trinetra,omitempty"`
	Permissions []plugin.Permission `json:"permissions"`
	Published   string              `json:"published"`
	Packages    []IndexPackage      `json:"packages"`
}

// IndexPackage is the downloadable package for one platform.
type IndexPackage struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// DecodeIndex strictly decodes and validates an index document.
func DecodeIndex(b []byte) (Index, error) {
	var ix Index
	if len(b) > MaxIndexBytes {
		return ix, fmt.Errorf("registry: index larger than %d bytes", MaxIndexBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ix); err != nil {
		return Index{}, fmt.Errorf("registry: index: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Index{}, errors.New("registry: index: trailing bytes after the JSON document")
	}
	if err := ix.validate(); err != nil {
		return Index{}, err
	}
	return ix, nil
}

func (ix Index) validate() error {
	if ix.Schema != IndexSchema {
		return fmt.Errorf("registry: index schema %d is not supported", ix.Schema)
	}
	if ix.Serial < 0 {
		return errors.New("registry: index serial is negative")
	}
	if _, err := time.Parse(time.RFC3339, ix.Issued); err != nil {
		return fmt.Errorf("registry: index issued: %w", err)
	}
	if _, err := time.Parse(time.RFC3339, ix.Expires); err != nil {
		return fmt.Errorf("registry: index expires: %w", err)
	}
	ids := map[string]bool{}
	for _, p := range ix.Plugins {
		if !plugin.IDPattern.MatchString(p.ID) {
			return fmt.Errorf("registry: index: bad plugin id %q", p.ID)
		}
		if ids[p.ID] {
			return fmt.Errorf("registry: index: duplicate plugin %q", p.ID)
		}
		ids[p.ID] = true
		if err := p.validate(); err != nil {
			return err
		}
	}
	for _, r := range ix.Revoked {
		if !plugin.IDPattern.MatchString(r.ID) {
			return fmt.Errorf("registry: index: bad revoked id %q", r.ID)
		}
		for _, v := range r.Versions {
			if _, err := update.ParseVersion(v); err != nil {
				return fmt.Errorf("registry: index: revoked %s: %w", r.ID, err)
			}
		}
	}
	return nil
}

func (p IndexPlugin) validate() error {
	seen := map[string]bool{}
	for _, v := range p.Versions {
		pv, err := update.ParseVersion(v.Version)
		if err != nil {
			return fmt.Errorf("registry: index: %s: %w", p.ID, err)
		}
		if seen[pv.String()] {
			return fmt.Errorf("registry: index: %s: duplicate version %s", p.ID, v.Version)
		}
		seen[pv.String()] = true
		if _, _, err := plugin.ParseHostAPI(v.HostAPI); err != nil {
			return fmt.Errorf("registry: index: %s %s: %w", p.ID, v.Version, err)
		}
		for _, pk := range v.Packages {
			if err := pk.validate(); err != nil {
				return fmt.Errorf("registry: index: %s %s: %w", p.ID, v.Version, err)
			}
		}
	}
	return nil
}

func (pk IndexPackage) validate() error {
	u, err := url.Parse(pk.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("package url %q must be https", pk.URL)
	}
	if !sha256Hex.MatchString(pk.SHA256) {
		return fmt.Errorf("package sha256 for %s/%s must be 64 lowercase hex characters", pk.OS, pk.Arch)
	}
	if pk.Size <= 0 {
		return fmt.Errorf("package size for %s/%s must be positive", pk.OS, pk.Arch)
	}
	if pk.OS != "linux" && pk.OS != "darwin" {
		return fmt.Errorf("package os %q is not supported", pk.OS)
	}
	if pk.Arch != "amd64" && pk.Arch != "arm64" && pk.Arch != "arm" {
		return fmt.Errorf("package arch %q is not supported", pk.Arch)
	}
	return nil
}
