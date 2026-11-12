// Package pluginrt installs, verifies, stores and supervises plugin packages.
package pluginrt

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	MaxPackageBytes   = 64 << 20
	MaxExtractedBytes = 256 << 20
	MaxPackageEntries = 64
	maxSigBytes       = 4096
)

var (
	maxPackageBytes   int64 = MaxPackageBytes
	maxExtractedBytes int64 = MaxExtractedBytes
)

var (
	binEntryRe  = regexp.MustCompile(`^bin/([a-z0-9]+)-([a-z0-9]+)/(trinetra-plugin-[a-z][a-z0-9-]{1,31})$`)
	fileEntryRe = regexp.MustCompile(`^files/([a-z0-9][a-z0-9._-]{0,63})$`)
)

// Package is an extracted, not yet trusted plugin package.
type Package struct {
	Dir           string
	ManifestBytes []byte
	Sig           []byte
	Manifest      plugin.Manifest
	SHA256        string
	Size          int64
}

type countingReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.n > c.max {
		return n, fmt.Errorf("package: larger than %d bytes", c.max)
	}
	return n, err
}

// ReadPackage hashes and extracts a tar.gz into dst (which must be empty), allowing only plugin.json, plugin.json.sig, bin/<os>-<arch>/trinetra-plugin-<id>, files/<name>.
func ReadPackage(r io.Reader, dst string) (Package, error) {
	ents, err := os.ReadDir(dst)
	if err != nil {
		return Package{}, fmt.Errorf("package: %w", err)
	}
	if len(ents) != 0 {
		return Package{}, errors.New("package: destination is not empty")
	}
	p, err := extract(r, dst)
	if err != nil {
		clearDir(dst)
		return Package{}, err
	}
	return p, nil
}

func clearDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func cleanEntryName(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\\x00") {
		return false
	}
	return name != ".." && !strings.HasPrefix(name, "../")
}

func extract(r io.Reader, dst string) (Package, error) {
	cr := &countingReader{r: r, max: maxPackageBytes}
	h := sha256.New()
	tee := io.TeeReader(cr, h)
	gz, err := gzip.NewReader(tee)
	if err != nil {
		return Package{}, fmt.Errorf("package: not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var (
		seen      = map[string]bool{}
		total     int64
		manifestB []byte
		sig       []byte
		bins      []string
		files     []string
		count     int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Package{}, fmt.Errorf("package: %w", err)
		}
		count++
		if count > MaxPackageEntries {
			return Package{}, fmt.Errorf("package: more than %d entries", MaxPackageEntries)
		}
		name := hdr.Name
		if hdr.Typeflag == tar.TypeDir {
			if !cleanEntryName(strings.TrimSuffix(name, "/")) {
				return Package{}, fmt.Errorf("package: unsafe entry path %q", name)
			}
			continue
		}
		if !cleanEntryName(name) {
			return Package{}, fmt.Errorf("package: unsafe entry path %q", name)
		}
		if hdr.Typeflag != tar.TypeReg {
			return Package{}, fmt.Errorf("package: entry %q is not a regular file", name)
		}
		if seen[name] {
			return Package{}, fmt.Errorf("package: duplicate entry %q", name)
		}
		seen[name] = true
		mode := os.FileMode(0o644)
		limit := maxExtractedBytes - total
		switch {
		case name == "plugin.json":
			limit = min(limit, plugin.MaxManifestBytes)
		case name == "plugin.json.sig":
			limit = min(limit, maxSigBytes)
		case binEntryRe.MatchString(name):
			mode = 0o755
			bins = append(bins, name)
		case fileEntryRe.MatchString(name):
			files = append(files, fileEntryRe.FindStringSubmatch(name)[1])
		default:
			return Package{}, fmt.Errorf("package: unexpected entry %q", name)
		}
		if hdr.Size > limit {
			return Package{}, fmt.Errorf("package: entry %q too large", name)
		}
		full := filepath.Join(dst, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return Package{}, fmt.Errorf("package: %w", err)
		}
		f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return Package{}, fmt.Errorf("package: %w", err)
		}
		n, cerr := io.Copy(f, io.LimitReader(tr, limit+1))
		if e := f.Close(); cerr == nil {
			cerr = e
		}
		if cerr != nil {
			return Package{}, fmt.Errorf("package: %w", cerr)
		}
		if n > limit {
			return Package{}, fmt.Errorf("package: entry %q too large", name)
		}
		if err := os.Chmod(full, mode); err != nil {
			return Package{}, fmt.Errorf("package: %w", err)
		}
		total += n
		switch name {
		case "plugin.json":
			manifestB, err = os.ReadFile(full)
		case "plugin.json.sig":
			sig, err = os.ReadFile(full)
		}
		if err != nil {
			return Package{}, fmt.Errorf("package: %w", err)
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return Package{}, fmt.Errorf("package: %w", err)
	}
	if manifestB == nil {
		return Package{}, errors.New("package: plugin.json is missing")
	}
	m, err := plugin.DecodeManifest(manifestB)
	if err != nil {
		return Package{}, err
	}
	for _, b := range bins {
		if path.Base(b) != plugin.BinaryName(m.ID) {
			return Package{}, fmt.Errorf("package: binary %q does not belong to plugin %q", b, m.ID)
		}
	}
	declared := map[string]bool{}
	for _, f := range m.Contributes.Files {
		declared[f.Name] = true
	}
	for _, f := range files {
		if !declared[f] {
			return Package{}, fmt.Errorf("package: file %q is not declared in contributes.files", f)
		}
	}
	return Package{Dir: dst, ManifestBytes: manifestB, Sig: sig, Manifest: m,
		SHA256: hex.EncodeToString(h.Sum(nil)), Size: cr.n}, nil
}
