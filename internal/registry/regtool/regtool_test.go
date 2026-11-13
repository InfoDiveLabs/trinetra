package regtool

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
)

const manifestSrc = `{
  "schema": 1,
  "id": "hello",
  "name": "Hello",
  "version": "1.0.0",
  "publisher": "InfoDive",
  "description": "Says hello",
  "host_api": "^1.0",
  "permissions": ["metrics.publish"],
  "binaries": [],
  "contributes": {"files": [{"name": "report.txt", "title": "Report", "content_type": "text/plain"}]}
}`

func writeFile(t *testing.T, p string, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func newBuild(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "plugin.json"), strings.Replace(manifestSrc, "1.0.0", version, 1), 0o644)
	writeFile(t, filepath.Join(dir, "bin/linux-amd64/trinetra-plugin-hello"), "amd64-"+version, 0o755)
	writeFile(t, filepath.Join(dir, "bin/linux-arm64/trinetra-plugin-hello"), "arm64-"+version, 0o755)
	writeFile(t, filepath.Join(dir, "files/report.txt"), "report", 0o644)
	return dir
}

func signedBuild(t *testing.T, version string) (dir string, pub ed25519.PublicKey, keyPath string) {
	t.Helper()
	dir = newBuild(t, version)
	keyPath = filepath.Join(t.TempDir(), "key")
	pub, err := Keygen(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := FillHashes(dir, "plugin.json"); err != nil {
		t.Fatal(err)
	}
	if err := SignFile(keyPath, "manifest", filepath.Join(dir, "plugin.json"), filepath.Join(dir, "plugin.json.sig")); err != nil {
		t.Fatal(err)
	}
	return dir, pub, keyPath
}

func TestKeygenWritesPrivate0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	pub, err := Keygen(p)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode().Perm())
	}
	priv, err := LoadKey(p)
	if err != nil || !priv.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatalf("LoadKey mismatch: %v", err)
	}
	if _, err := Keygen(p); err == nil {
		t.Fatal("Keygen overwrote an existing key")
	}
	if len(Fingerprint(pub)) != 64 {
		t.Fatal("fingerprint length")
	}
}

func TestHashFillsBinaries(t *testing.T) {
	dir := newBuild(t, "1.0.0")
	m, err := FillHashes(dir, "plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Binaries) != 2 || m.Binaries[0].Arch != "amd64" || m.Binaries[1].Arch != "arm64" || m.Binaries[0].Size != int64(len("amd64-1.0.0")) {
		t.Fatalf("binaries = %+v", m.Binaries)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if _, err := plugin.DecodeManifest(b); err != nil {
		t.Fatalf("rewritten manifest invalid: %v", err)
	}
	os.RemoveAll(filepath.Join(dir, "bin"))
	if _, err := FillHashes(dir, "plugin.json"); err == nil {
		t.Fatal("manifest without binaries accepted")
	}
}

func TestPackageDeterministic(t *testing.T) {
	dir, _, _ := signedBuild(t, "1.0.0")
	out := t.TempDir()
	s1, n1, err := Package(dir, "linux", "amd64", filepath.Join(out, "a.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	s2, n2, err := Package(dir, "linux", "amd64", filepath.Join(out, "b.tgz"))
	if err != nil || s1 != s2 || n1 != n2 || n1 == 0 {
		t.Fatalf("not deterministic: %s %s %v", s1, s2, err)
	}
	a, _ := os.ReadFile(filepath.Join(out, "a.tgz"))
	b, _ := os.ReadFile(filepath.Join(out, "b.tgz"))
	if string(a) != string(b) {
		t.Fatal("bytes differ")
	}
}

func TestPackageRefusesStaleBinary(t *testing.T) {
	dir, _, _ := signedBuild(t, "1.0.0")
	writeFile(t, filepath.Join(dir, "bin/linux-amd64/trinetra-plugin-hello"), "changed", 0o755)
	if _, _, err := Package(dir, "linux", "amd64", filepath.Join(t.TempDir(), "x.tgz")); err == nil {
		t.Fatal("stale binary packaged")
	}
	if _, _, err := Package(dir, "linux", "arm", filepath.Join(t.TempDir(), "x.tgz")); err == nil {
		t.Fatal("missing platform packaged")
	}
}

func TestPackageReadableByPluginrt(t *testing.T) {
	dir, _, _ := signedBuild(t, "1.0.0")
	out := filepath.Join(t.TempDir(), "p.tgz")
	if _, _, err := Package(dir, "linux", "arm64", out); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if h.Typeflag != tar.TypeReg || h.Uid != 0 || h.Gid != 0 || h.ModTime.Unix() != 0 {
			t.Errorf("%s: header %+v", h.Name, h)
		}
		if path.IsAbs(h.Name) || strings.Contains(h.Name, "..") || path.Clean(h.Name) != h.Name {
			t.Errorf("unsafe path %q", h.Name)
		}
		want := os.FileMode(0o644)
		if strings.HasPrefix(h.Name, "bin/") {
			want = 0o755
		}
		if os.FileMode(h.Mode) != want {
			t.Errorf("%s mode %o", h.Name, h.Mode)
		}
	}
	want := []string{"bin/linux-arm64/trinetra-plugin-hello", "files/report.txt", "plugin.json", "plugin.json.sig"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v", names)
	}
}

func TestSignVerifyRoundTripWithInternalRegistry(t *testing.T) {
	dir, pub, _ := signedBuild(t, "1.0.0")
	mb, _ := os.ReadFile(filepath.Join(dir, "plugin.json"))
	sig, _ := os.ReadFile(filepath.Join(dir, "plugin.json.sig"))
	if err := registry.VerifyManifest([]ed25519.PublicKey{pub}, mb, sig); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(pub, "manifest", filepath.Join(dir, "plugin.json"), filepath.Join(dir, "plugin.json.sig")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFile(pub, "index", filepath.Join(dir, "plugin.json"), filepath.Join(dir, "plugin.json.sig")); !errors.Is(err, plugin.ErrBadSignature) {
		t.Fatalf("manifest sig accepted as index sig: %v", err)
	}

	now := time.Now()
	_, ib, err := BuildIndex(IndexOptions{Serial: 1, Lifetime: 21 * 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	kp := filepath.Join(t.TempDir(), "k2")
	pub2, _ := Keygen(kp)
	p2, _ := LoadKey(kp)
	isig := SignBytes(p2, "index", ib)
	if _, _, err := registry.VerifyIndex([]ed25519.PublicKey{pub2}, ib, isig, now, registry.State{}); err != nil {
		t.Fatal(err)
	}
	if SignBytes(p2, "bogus", ib) != nil {
		t.Fatal("unknown kind signed")
	}
	if _, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(isig))); err != nil {
		t.Fatal(err)
	}
}

func packBuild(t *testing.T, version, outDir string) {
	t.Helper()
	dir, _, _ := signedBuild(t, version)
	for _, arch := range []string{"amd64", "arm64"} {
		if _, _, err := Package(dir, "linux", arch, filepath.Join(outDir, "hello-"+version+"-linux-"+arch+".tgz")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndexRefusesNonIncreasingSerial(t *testing.T) {
	pk := t.TempDir()
	packBuild(t, "1.0.0", pk)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	ix, _, err := BuildIndex(IndexOptions{PackagesDir: pk, BaseURL: "https://example.com/dl/{id}-v{version}", Serial: 5, Lifetime: 21 * 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []int64{5, 4} {
		if _, _, err := BuildIndex(IndexOptions{Prev: &ix, Serial: s, Lifetime: 24 * time.Hour, Now: now}); err == nil {
			t.Fatalf("serial %d accepted", s)
		}
	}
	if _, _, err := BuildIndex(IndexOptions{Prev: &ix, Serial: 6, Lifetime: 24 * time.Hour, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := BuildIndex(IndexOptions{Serial: 1, Lifetime: 60 * 24 * time.Hour, Now: now}); err == nil {
		t.Fatal("over-long lifetime accepted")
	}
}

func TestIndexMergesPrevious(t *testing.T) {
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	pk1 := t.TempDir()
	packBuild(t, "1.0.0", pk1)
	ix1, _, err := BuildIndex(IndexOptions{PackagesDir: pk1, BaseURL: "https://example.com/dl/{id}-v{version}", Serial: 1, Lifetime: 21 * 24 * time.Hour, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	pk2 := t.TempDir()
	packBuild(t, "1.1.0", pk2)
	rev := []plugin.Revocation{{ID: "hello", Versions: []string{"1.0.0"}, Reason: "bug"}}
	ix2, raw, err := BuildIndex(IndexOptions{PackagesDir: pk2, BaseURL: "https://example.com/dl/{id}-v{version}", Serial: 2, Lifetime: 21 * 24 * time.Hour, Prev: &ix1, Revoked: rev, Now: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(ix2.Plugins) != 1 || len(ix2.Plugins[0].Versions) != 2 || ix2.Plugins[0].Versions[0].Version != "1.0.0" || ix2.Plugins[0].Versions[1].Version != "1.1.0" {
		t.Fatalf("versions = %+v", ix2.Plugins)
	}
	v := ix2.Plugins[0].Versions[1]
	if len(v.Packages) != 2 || v.Packages[0].Arch != "amd64" || v.Packages[0].URL != "https://example.com/dl/hello-v1.1.0/hello-1.1.0-linux-amd64.tgz" {
		t.Fatalf("packages = %+v", v.Packages)
	}
	if v.Published != "2026-11-13" || ix2.Plugins[0].Versions[0].Published != "2026-11-13" || len(ix2.Revoked) != 1 {
		t.Fatalf("ix2 = %+v", ix2)
	}
	if _, err := registry.DecodeIndex(raw); err != nil {
		t.Fatal(err)
	}
	ix3, _, err := BuildIndex(IndexOptions{Serial: 3, Lifetime: 24 * time.Hour, Prev: &ix2, Now: now})
	if err != nil || len(ix3.Revoked) != 1 || len(ix3.Plugins[0].Versions) != 2 {
		t.Fatalf("resign did not carry content: %+v %v", ix3, err)
	}
}

func TestCheck(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "hello")
	writeFile(t, filepath.Join(good, "plugin.json"), manifestSrc, 0o644)
	if err := Check(good); err != nil {
		t.Fatalf("good: %v", err)
	}
	wrongDir := filepath.Join(root, "other")
	writeFile(t, filepath.Join(wrongDir, "plugin.json"), manifestSrc, 0o644)
	if err := Check(wrongDir); err == nil {
		t.Fatal("dir/id mismatch accepted")
	}
	tmpl := filepath.Join(root, "_template")
	writeFile(t, filepath.Join(tmpl, "plugin.json"), manifestSrc, 0o644)
	if err := Check(tmpl); err != nil {
		t.Fatalf("template: %v", err)
	}
	future := filepath.Join(root, "future")
	writeFile(t, filepath.Join(future, "plugin.json"), strings.Replace(strings.Replace(manifestSrc, `"hello"`, `"future"`, 1), "^1.0", "^7.0", 1), 0o644)
	if err := Check(future); !errors.Is(err, plugin.ErrIncompatible) {
		t.Fatalf("host_api: %v", err)
	}
	bad := filepath.Join(root, "bad")
	writeFile(t, filepath.Join(bad, "plugin.json"), `{"schema":1}`, 0o644)
	if err := Check(bad); err == nil {
		t.Fatal("invalid manifest accepted")
	}
}

func TestParseLifetime(t *testing.T) {
	if d, err := ParseLifetime("21d"); err != nil || d != 21*24*time.Hour {
		t.Fatalf("%v %v", d, err)
	}
	if d, err := ParseLifetime("48h"); err != nil || d != 48*time.Hour {
		t.Fatalf("%v %v", d, err)
	}
	for _, bad := range []string{"", "x", "0d", "-1d"} {
		if _, err := ParseLifetime(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPackageUnsignedForCommunityShare(t *testing.T) {
	dir := newBuild(t, "1.0.0")
	if _, err := FillHashes(dir, "plugin.json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Package(dir, "linux", "amd64", filepath.Join(t.TempDir(), "x.tgz")); err == nil {
		t.Fatal("registry package built without a signature")
	}
	out := filepath.Join(t.TempDir(), "u.tgz")
	if _, _, err := PackageUnsigned(dir, "linux", "amd64", out); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "bin/linux-amd64/trinetra-plugin-hello,files/report.txt,plugin.json" {
		t.Fatalf("entries %v", names)
	}
}
