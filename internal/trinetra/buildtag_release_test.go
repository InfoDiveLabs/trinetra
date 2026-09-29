//go:build !trinetra_testkeys

package trinetra

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseBinariesCarryNoTestKeys is R22: a default (release) build of
// the daemon and of trinetra-release must contain neither the deterministic
// test signers/key set nor the e2e crash-on-start hook. Only a
// trinetra_testkeys build (the e2e image) may carry them.
func TestReleaseBinariesCarryNoTestKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, pkg := range []string{"./cmd/trinetra", "./cmd/trinetra-release"} {
		bin := filepath.Join(out, filepath.Base(pkg))
		build := exec.Command("go", "build", "-o", bin, pkg)
		build.Dir = root
		if b, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, b)
		}
		nm := exec.Command("go", "tool", "nm", bin)
		nm.Dir = root
		syms, err := nm.CombinedOutput()
		if err != nil {
			t.Fatalf("go tool nm %s: %v\n%s", pkg, err, syms)
		}
		for _, line := range strings.Split(string(syms), "\n") {
			for _, bad := range []string{"TestSigner", "TestKeySet", "e2eCrashOnStart", "internal/update/updatetest"} {
				if strings.Contains(line, bad) {
					t.Errorf("%s release build contains %q: %s", pkg, bad, strings.TrimSpace(line))
				}
			}
		}
	}
}
