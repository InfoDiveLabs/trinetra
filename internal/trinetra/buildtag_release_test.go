//go:build !trinetra_testkeys

package trinetra

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseBinariesCarryNoTestKeys: a default (release) build of the daemon.
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
		// No update-e2e hook can be read from the environment by a release binary: the
		// default-build hooks never look a TRINETRA_E2E_* variable up.
		raw, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		if i := bytes.Index(raw, []byte("TRINETRA_E2E_")); i >= 0 {
			end := i + 48
			if end > len(raw) {
				end = len(raw)
			}
			t.Errorf("%s release build contains an e2e hook variable name: %q", pkg, raw[i:end])
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
