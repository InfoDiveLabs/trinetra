package regtool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func decodeLoose(b []byte) (plugin.Manifest, error) {
	var m plugin.Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return m, errors.New("manifest: trailing bytes after the JSON document")
	}
	return m, nil
}

func resolveManifest(buildDir, manifestPath string) string {
	if filepath.IsAbs(manifestPath) {
		return manifestPath
	}
	if p := filepath.Join(buildDir, manifestPath); fileExists(p) {
		return p
	}
	return manifestPath
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// FillHashes rewrites manifestPath's binaries[] from <buildDir>/bin/<os>-<arch>/trinetra-plugin-<id> and validates it.
func FillHashes(buildDir, manifestPath string) (plugin.Manifest, error) {
	manifestPath = resolveManifest(buildDir, manifestPath)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return plugin.Manifest{}, err
	}
	m, err := decodeLoose(raw)
	if err != nil {
		return plugin.Manifest{}, err
	}
	m.Binaries = nil
	dirs, err := filepath.Glob(filepath.Join(buildDir, "bin", "*-*"))
	if err != nil {
		return plugin.Manifest{}, err
	}
	for _, d := range dirs {
		goos, goarch, ok := strings.Cut(filepath.Base(d), "-")
		if !ok {
			continue
		}
		bin := filepath.Join(d, plugin.BinaryName(m.ID))
		if !fileExists(bin) {
			continue
		}
		sum, size, err := hashFile(bin)
		if err != nil {
			return plugin.Manifest{}, err
		}
		m.Binaries = append(m.Binaries, plugin.Binary{OS: goos, Arch: goarch, SHA256: sum, Size: size})
	}
	sort.Slice(m.Binaries, func(i, j int) bool {
		a, b := m.Binaries[i], m.Binaries[j]
		return a.OS+"/"+a.Arch < b.OS+"/"+b.Arch
	})
	if err := m.Validate(); err != nil {
		return plugin.Manifest{}, err
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return plugin.Manifest{}, err
	}
	if err := os.WriteFile(manifestPath, append(out, '\n'), 0o644); err != nil {
		return plugin.Manifest{}, err
	}
	return m, nil
}
