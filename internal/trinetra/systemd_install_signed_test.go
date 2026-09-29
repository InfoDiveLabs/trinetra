package trinetra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func TestVerifyInstallBundle(t *testing.T) {
	dir := t.TempDir()
	core := []byte("CORE")
	os.WriteFile(filepath.Join(dir, "trinetra"), core, 0o755)
	m := update.Manifest{Schema: 1, Product: "trinetra", Version: "0.5.0", Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.4.1",
		Files: []update.File{mf("trinetra-linux-"+runtime.GOARCH, core)}}
	m.Files[0].Arch = runtime.GOARCH
	mb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), update.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), update.NewTestSigner(2).SignRelease(mb), 0o644)

	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err != nil {
		t.Fatalf("valid bundle refused: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "trinetra"), []byte("EVIL"), 0o755)
	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err == nil || !strings.Contains(err.Error(), "does not match the signed manifest") {
		t.Fatalf("tampered binary: %v", err)
	}
	os.Remove(filepath.Join(dir, "manifest.maint.sig"))
	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err == nil {
		t.Fatal("missing maintainer sig accepted")
	}
}

// TestVerifyInstallBundleNoManifest pins errNoSignedManifest: absent
// manifest.json must be distinguishable from a present-but-invalid one, so
// cmdInstall (verifyInstallSignature) can warn-and-continue instead of
// refusing outright unless --require-signed was passed.
func TestVerifyInstallBundleNoManifest(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "trinetra")
	os.WriteFile(self, []byte("CORE"), 0o755)

	_, err := verifyInstallBundle(testKeys(), self, []string{"trinetra"})
	if err != errNoSignedManifest {
		t.Fatalf("err = %v, want errNoSignedManifest", err)
	}
}
