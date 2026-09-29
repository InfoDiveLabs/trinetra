package update

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type File struct {
	Name   string `json:"name"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type ManifestKeys struct {
	CI      []string `json:"ci"`
	Maint   []string `json:"maint"`
	Pointer []string `json:"pointer"`
}

type Manifest struct {
	Schema         int          `json:"schema"`
	Product        string       `json:"product"`
	Version        string       `json:"version"`
	Channel        string       `json:"channel"`
	Published      string       `json:"published"`
	MinUpgradeFrom string       `json:"min_upgrade_from"`
	Keys           ManifestKeys `json:"keys"`
	Files          []File       `json:"files"`
}

type Pointer struct {
	Schema  int    `json:"schema"`
	Product string `json:"product"`
	Channel string `json:"channel"`
	Version string `json:"version"`
	Issued  string `json:"issued"`
	Expires string `json:"expires"`
}

// decodeStrict rejects unknown fields and any bytes after the first value.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	return nil
}

func validChannel(c string) bool { return c == "stable" || c == "beta" }

// DecodeManifest parses and validates a manifest. It does NOT check
// signatures; VerifyRelease does both.
func DecodeManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := decodeStrict(b, &m); err != nil {
		return Manifest{}, err
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("%w: schema %d", ErrMalformed, m.Schema)
	}
	if m.Product != "trinetra" {
		return Manifest{}, fmt.Errorf("%w: %q", ErrWrongProduct, m.Product)
	}
	if !validChannel(m.Channel) {
		return Manifest{}, fmt.Errorf("%w: channel %q", ErrMalformed, m.Channel)
	}
	v, err := ParseVersion(m.Version)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if v.Pre != "" && m.Channel == "stable" {
		return Manifest{}, fmt.Errorf("%w: pre-release %s on stable", ErrMalformed, m.Version)
	}
	if _, err := ParseVersion(m.MinUpgradeFrom); err != nil {
		return Manifest{}, fmt.Errorf("%w: min_upgrade_from: %v", ErrMalformed, err)
	}
	if _, err := time.Parse(time.RFC3339, m.Published); err != nil {
		return Manifest{}, fmt.Errorf("%w: published: %v", ErrMalformed, err)
	}
	if len(m.Files) == 0 {
		return Manifest{}, fmt.Errorf("%w: no files", ErrMalformed)
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if f.Name == "" || strings.ContainsAny(f.Name, `/\`) || f.Name == "." || f.Name == ".." || seen[f.Name] {
			return Manifest{}, fmt.Errorf("%w: file name %q", ErrMalformed, f.Name)
		}
		seen[f.Name] = true
		if f.OS != "linux" || (f.Arch != "amd64" && f.Arch != "arm64" && f.Arch != "arm") {
			return Manifest{}, fmt.Errorf("%w: %s: platform %s/%s", ErrMalformed, f.Name, f.OS, f.Arch)
		}
		if f.Size <= 0 {
			return Manifest{}, fmt.Errorf("%w: %s: size %d", ErrMalformed, f.Name, f.Size)
		}
		if h, err := hex.DecodeString(f.SHA256); err != nil || len(h) != 32 {
			return Manifest{}, fmt.Errorf("%w: %s: sha256", ErrMalformed, f.Name)
		}
	}
	return m, nil
}

// DecodePointer parses and validates a channel pointer (no signature check).
func DecodePointer(b []byte) (Pointer, error) {
	var p Pointer
	if err := decodeStrict(b, &p); err != nil {
		return Pointer{}, err
	}
	if p.Schema != 1 {
		return Pointer{}, fmt.Errorf("%w: schema %d", ErrMalformed, p.Schema)
	}
	if p.Product != "trinetra" {
		return Pointer{}, fmt.Errorf("%w: %q", ErrWrongProduct, p.Product)
	}
	if !validChannel(p.Channel) {
		return Pointer{}, fmt.Errorf("%w: channel %q", ErrMalformed, p.Channel)
	}
	if _, err := ParseVersion(p.Version); err != nil {
		return Pointer{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	for _, ts := range []string{p.Issued, p.Expires} {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return Pointer{}, fmt.Errorf("%w: time %q", ErrMalformed, ts)
		}
	}
	return p, nil
}
