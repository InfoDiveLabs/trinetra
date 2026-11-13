package regtool

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

type packEntry struct {
	name string
	mode int64
	data []byte
}

// Package writes a deterministic tar.gz of one platform's plugin package and returns its sha256 and size.
func Package(buildDir, goos, goarch, out string) (string, int64, error) {
	return pack(buildDir, goos, goarch, out, true)
}

// PackageUnsigned is Package without a manifest signature; hosts install the result as unverified.
func PackageUnsigned(buildDir, goos, goarch, out string) (string, int64, error) {
	return pack(buildDir, goos, goarch, out, false)
}

func pack(buildDir, goos, goarch, out string, signed bool) (string, int64, error) {
	mb, err := os.ReadFile(filepath.Join(buildDir, "plugin.json"))
	if err != nil {
		return "", 0, err
	}
	m, err := plugin.DecodeManifest(mb)
	if err != nil {
		return "", 0, err
	}
	var sigs []packEntry
	if signed {
		for _, name := range []string{plugin.ManifestCISigName, plugin.ManifestMaintSigName} {
			b, err := os.ReadFile(filepath.Join(buildDir, name))
			if err != nil {
				return "", 0, fmt.Errorf("regtool: a registry package needs CI and maintainer signatures (use -unsigned to share outside the registry): %w", err)
			}
			sigs = append(sigs, packEntry{name, 0o644, b})
		}
	}
	want, ok := m.BinaryFor(goos, goarch)
	if !ok {
		return "", 0, fmt.Errorf("regtool: manifest has no binary for %s/%s", goos, goarch)
	}
	binRel := plugin.BinaryPath(goos, goarch, m.ID)
	bin, err := os.ReadFile(filepath.Join(buildDir, filepath.FromSlash(binRel)))
	if err != nil {
		return "", 0, err
	}
	if sum := sha256.Sum256(bin); hex.EncodeToString(sum[:]) != want.SHA256 || int64(len(bin)) != want.Size {
		return "", 0, fmt.Errorf("regtool: %s does not match the manifest hash; run hash again", binRel)
	}
	entries := []packEntry{{"plugin.json", 0o644, mb}, {binRel, 0o755, bin}}
	entries = append(entries, sigs...)
	files, err := filepath.Glob(filepath.Join(buildDir, "files", "*"))
	if err != nil {
		return "", 0, err
	}
	sort.Strings(files)
	for _, f := range files {
		st, err := os.Lstat(f)
		if err != nil {
			return "", 0, err
		}
		if !st.Mode().IsRegular() {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return "", 0, err
		}
		entries = append(entries, packEntry{"files/" + filepath.Base(f), 0o644, b})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name: e.name, Mode: e.mode, Size: int64(len(e.data)), ModTime: time.Unix(0, 0),
			Typeflag: tar.TypeReg, Format: tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", 0, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if err := update.WriteFileAtomic(out, buf.Bytes(), 0o644); err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(buf.Bytes())
	return hex.EncodeToString(sum[:]), int64(buf.Len()), nil
}
