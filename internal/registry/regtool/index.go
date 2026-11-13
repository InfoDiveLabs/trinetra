package regtool

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// IndexOptions configures BuildIndex.
type IndexOptions struct {
	PackagesDir string // *.tgz produced by Package; optional
	BaseURL     string // https prefix; {id} and {version} are substituted, the file name is appended
	Serial      int64
	Lifetime    time.Duration
	Prev        *registry.Index
	Revoked     []plugin.Revocation // nil keeps Prev's list
	Now         time.Time
	// Keys, when set, must verify each package's CI and maintainer manifest signatures.
	Keys update.KeySet
}

type pkgInfo struct {
	manifestBytes []byte
	sigs          plugin.ManifestSigs
	manifest      plugin.Manifest
	goos          string
	goarch        string
	file          string
	sha           string
	size          int64
}

// ParseLifetime parses durations with a "d" day suffix ("21d") as well as time.ParseDuration forms.
func ParseLifetime(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("regtool: bad lifetime %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("regtool: bad lifetime %q", s)
	}
	return d, nil
}

func readPackage(file string) (pkgInfo, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return pkgInfo{}, err
	}
	sum := sha256.Sum256(raw)
	info := pkgInfo{file: filepath.Base(file), sha: hex.EncodeToString(sum[:]), size: int64(len(raw))}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return pkgInfo{}, fmt.Errorf("regtool: %s: %w", file, err)
	}
	tr := tar.NewReader(gz)
	var mb []byte
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return pkgInfo{}, fmt.Errorf("regtool: %s: %w", file, err)
		}
		switch {
		case h.Name == "plugin.json":
			if mb, err = io.ReadAll(io.LimitReader(tr, plugin.MaxManifestBytes+1)); err != nil {
				return pkgInfo{}, err
			}
		case h.Name == plugin.ManifestCISigName:
			if info.sigs.CI, err = io.ReadAll(io.LimitReader(tr, 4096)); err != nil {
				return pkgInfo{}, err
			}
		case h.Name == plugin.ManifestMaintSigName:
			if info.sigs.Maint, err = io.ReadAll(io.LimitReader(tr, 4096)); err != nil {
				return pkgInfo{}, err
			}
		case strings.HasPrefix(h.Name, "bin/"):
			parts := strings.Split(path.Dir(h.Name), "/")
			if len(parts) == 2 {
				info.goos, info.goarch, _ = strings.Cut(parts[1], "-")
			}
		}
	}
	m, err := plugin.DecodeManifest(mb)
	if err != nil {
		return pkgInfo{}, fmt.Errorf("regtool: %s: %w", file, err)
	}
	if info.goos == "" || info.goarch == "" {
		return pkgInfo{}, fmt.Errorf("regtool: %s: no binary entry", file)
	}
	info.manifest, info.manifestBytes = m, mb
	return info, nil
}

// BuildIndex merges the packages in o.PackagesDir into o.Prev and returns the new index and its encoded bytes.
func BuildIndex(o IndexOptions) (registry.Index, []byte, error) {
	var ix registry.Index
	if o.Prev != nil {
		if o.Serial <= o.Prev.Serial {
			return ix, nil, fmt.Errorf("regtool: serial %d must be greater than the previous serial %d", o.Serial, o.Prev.Serial)
		}
		b, err := json.Marshal(o.Prev)
		if err != nil {
			return ix, nil, err
		}
		if err := json.Unmarshal(b, &ix); err != nil {
			return ix, nil, err
		}
	}
	if o.Lifetime <= 0 || o.Lifetime > registry.MaxIndexLifetime {
		return ix, nil, fmt.Errorf("regtool: lifetime must be within (0, %s]", registry.MaxIndexLifetime)
	}
	ix.Schema = registry.IndexSchema
	ix.Serial = o.Serial
	ix.Issued = o.Now.UTC().Format(time.RFC3339)
	ix.Expires = o.Now.Add(o.Lifetime).UTC().Format(time.RFC3339)
	if o.Revoked != nil {
		ix.Revoked = o.Revoked
	}

	var pkgs []pkgInfo
	if o.PackagesDir != "" {
		files, err := filepath.Glob(filepath.Join(o.PackagesDir, "*.tgz"))
		if err != nil {
			return ix, nil, err
		}
		sort.Strings(files)
		for _, f := range files {
			p, err := readPackage(f)
			if err != nil {
				return ix, nil, err
			}
			if len(o.Keys.CI) > 0 || len(o.Keys.Maint) > 0 {
				if err := registry.VerifyManifest(o.Keys, p.manifestBytes, p.sigs); err != nil {
					return ix, nil, fmt.Errorf("regtool: %s: %w", p.file, err)
				}
			}
			pkgs = append(pkgs, p)
		}
	}
	for _, p := range pkgs {
		if err := mergePackage(&ix, p, o); err != nil {
			return ix, nil, err
		}
	}
	if ix.Plugins == nil {
		ix.Plugins = []registry.IndexPlugin{}
	}
	sortIndex(&ix)
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		return ix, nil, err
	}
	b = append(b, '\n')
	if _, err := registry.DecodeIndex(b); err != nil {
		return ix, nil, err
	}
	return ix, b, nil
}

func mergePackage(ix *registry.Index, p pkgInfo, o IndexOptions) error {
	m := p.manifest
	url := strings.NewReplacer("{id}", m.ID, "{version}", m.Version).Replace(o.BaseURL)
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}
	pkg := registry.IndexPackage{OS: p.goos, Arch: p.goarch, URL: url + p.file, SHA256: p.sha, Size: p.size}

	pi := -1
	for i := range ix.Plugins {
		if ix.Plugins[i].ID == m.ID {
			pi = i
		}
	}
	if pi < 0 {
		ix.Plugins = append(ix.Plugins, registry.IndexPlugin{ID: m.ID})
		pi = len(ix.Plugins) - 1
	}
	ip := &ix.Plugins[pi]
	ip.Name, ip.Publisher, ip.Description, ip.Homepage = m.Name, m.Publisher, m.Description, m.Homepage

	vi := -1
	for i := range ip.Versions {
		if ip.Versions[i].Version == m.Version {
			vi = i
		}
	}
	if vi < 0 {
		ip.Versions = append(ip.Versions, registry.IndexVersion{Version: m.Version, Published: o.Now.UTC().Format("2006-01-02")})
		vi = len(ip.Versions) - 1
	}
	iv := &ip.Versions[vi]
	iv.HostAPI, iv.MinTrinetra, iv.Permissions = m.HostAPI, m.MinTrinetra, m.Permissions
	if iv.Permissions == nil {
		iv.Permissions = []plugin.Permission{}
	}
	for i := range iv.Packages {
		if iv.Packages[i].OS == pkg.OS && iv.Packages[i].Arch == pkg.Arch {
			iv.Packages[i] = pkg
			return nil
		}
	}
	iv.Packages = append(iv.Packages, pkg)
	return nil
}

func sortIndex(ix *registry.Index) {
	sort.Slice(ix.Plugins, func(i, j int) bool { return ix.Plugins[i].ID < ix.Plugins[j].ID })
	for i := range ix.Plugins {
		vs := ix.Plugins[i].Versions
		sort.Slice(vs, func(a, b int) bool {
			va, _ := update.ParseVersion(vs[a].Version)
			vb, _ := update.ParseVersion(vs[b].Version)
			return update.CompareVersions(va, vb) < 0
		})
		for j := range vs {
			ps := vs[j].Packages
			sort.Slice(ps, func(a, b int) bool { return ps[a].OS+"/"+ps[a].Arch < ps[b].OS+"/"+ps[b].Arch })
		}
	}
}
