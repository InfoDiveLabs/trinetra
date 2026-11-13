package pluginrt

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func mustFail(t *testing.T, data []byte) string {
	t.Helper()
	dst := t.TempDir()
	_, err := ReadPackage(bytes.NewReader(data), dst)
	if err == nil {
		t.Fatal("expected error")
	}
	ents, _ := os.ReadDir(dst)
	if len(ents) != 0 {
		t.Fatalf("dst not empty after failure: %v", ents)
	}
	return err.Error()
}

func TestReadPackageExtractsManifestSigBinary(t *testing.T) {
	m := testManifest("demo")
	data := buildPackage(t, packageFiles(t, m, true))
	dst := t.TempDir()
	p, err := ReadPackage(bytes.NewReader(data), dst)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.ID != "demo" || len(p.Sigs.CI) == 0 || len(p.Sigs.Maint) == 0 || len(p.ManifestBytes) == 0 || p.Size != int64(len(data)) {
		t.Fatalf("package = %+v", p)
	}
	bin := filepath.Join(dst, "bin", "linux-amd64", "trinetra-plugin-demo")
	st, err := os.Stat(bin)
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("binary: %v %v", st, err)
	}
	st, _ = os.Stat(filepath.Join(dst, "plugin.json"))
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("manifest mode %v", st.Mode())
	}
}

func TestReadPackageRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../x", "/etc/x", "bin/../../x"} {
		files := packageFiles(t, testManifest("demo"), false)
		files[name] = []byte("x")
		mustFail(t, buildPackage(t, files))
	}
}

func TestReadPackageRejectsSymlinkAndHardlink(t *testing.T) {
	for _, tf := range []byte{tar.TypeSymlink, tar.TypeLink} {
		ents := []rawEntry{{hdr: tar.Header{Name: "bin/linux-amd64/trinetra-plugin-demo", Typeflag: tf, Linkname: "/etc/passwd"}}}
		mustFail(t, buildRaw(t, ents))
	}
}

func TestReadPackageRejectsUnexpectedPath(t *testing.T) {
	for _, name := range []string{"README", "bin/linux-amd64/other", "files/sub/x"} {
		files := packageFiles(t, testManifest("demo"), false)
		files[name] = []byte("x")
		mustFail(t, buildPackage(t, files))
	}
}

func TestReadPackageRejectsDuplicateEntry(t *testing.T) {
	files := packageFiles(t, testManifest("demo"), false)
	ents := []rawEntry{{hdr: tar.Header{Name: "plugin.json"}, body: files["plugin.json"]}, {hdr: tar.Header{Name: "plugin.json"}, body: files["plugin.json"]}}
	mustFail(t, buildRaw(t, ents))
}

func TestReadPackageRejectsTooManyEntries(t *testing.T) {
	m := testManifest("demo")
	for i := 0; i < MaxPackageEntries+1; i++ {
		m.Contributes.Files = append(m.Contributes.Files, plugin.FileDecl{Name: "f" + strconv.Itoa(i), Title: "t", ContentType: "text/plain"})
	}
	files := packageFiles(t, m, false)
	for _, f := range m.Contributes.Files {
		files["files/"+f.Name] = []byte("x")
	}
	mustFail(t, buildPackage(t, files))
}

func TestReadPackageRejectsCompressedOversize(t *testing.T) {
	prev := maxPackageBytes
	maxPackageBytes = 50
	t.Cleanup(func() { maxPackageBytes = prev })
	mustFail(t, buildPackage(t, packageFiles(t, testManifest("demo"), false)))
}

func TestReadPackageRejectsExtractedOversize(t *testing.T) {
	prev := maxExtractedBytes
	maxExtractedBytes = 8
	t.Cleanup(func() { maxExtractedBytes = prev })
	mustFail(t, buildPackage(t, packageFiles(t, testManifest("demo"), false)))
}

func TestReadPackageRejectsUndeclaredFile(t *testing.T) {
	files := packageFiles(t, testManifest("demo"), false)
	files["files/x"] = []byte("x")
	mustFail(t, buildPackage(t, files))
}

func TestReadPackageAcceptsDeclaredFile(t *testing.T) {
	m := testManifest("demo", func(m *plugin.Manifest) {
		m.Contributes.Files = []plugin.FileDecl{{Name: "x", Title: "X", ContentType: "text/plain"}}
	})
	files := packageFiles(t, m, false)
	files["files/x"] = []byte("hello")
	dst := t.TempDir()
	if _, err := ReadPackage(bytes.NewReader(buildPackage(t, files)), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "files", "x")); string(b) != "hello" {
		t.Fatalf("file = %q", b)
	}
}

func TestReadPackageSHA256IsOfCompressedBytes(t *testing.T) {
	data := buildPackage(t, packageFiles(t, testManifest("demo"), false))
	p, err := ReadPackage(bytes.NewReader(data), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if p.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("sha %s", p.SHA256)
	}
}

func TestReadPackageRejectsNonEmptyDst(t *testing.T) {
	dst := t.TempDir()
	_ = os.WriteFile(filepath.Join(dst, "x"), nil, 0o644)
	if _, err := ReadPackage(bytes.NewReader(buildPackage(t, packageFiles(t, testManifest("demo"), false))), dst); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(filepath.Join(dst, "x")); err != nil {
		t.Fatal("pre-existing file removed")
	}
}

func TestReadPackageRejectsMissingManifestAndForeignBinary(t *testing.T) {
	mustFail(t, buildPackage(t, map[string][]byte{"bin/linux-amd64/trinetra-plugin-demo": testBinary}))
	files := packageFiles(t, testManifest("demo"), false)
	files["bin/linux-arm64/trinetra-plugin-other"] = []byte("x")
	mustFail(t, buildPackage(t, files))
}
