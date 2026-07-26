package serverwatch

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultBuildIsStdlibOnly asserts the DEFAULT (untagged) build imports
// no third-party packages: the web build tag (see web_deps.go/daemon_web.go/
// daemon_noweb.go) must keep go-webauthn, x/crypto/autocert, etc. entirely
// out of the module graph unless built with `-tags web`. This is the guard
// that makes the seam's isolation promise checkable rather than aspirational.
//
// `go test` runs this package's tests with cwd == this directory
// (internal/serverwatch), but "./..." must expand from the MODULE ROOT to
// cover every package (including cmd/serverwatch, internal/config,
// internal/telegram) rather than just this package's own subtree — so the
// command's working directory is explicitly set to the module root
// (two levels up from this file) rather than relying on the test binary's
// default cwd.
func TestDefaultBuildIsStdlibOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go list in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, p := range strings.Fields(string(out)) {
		// stdlib import paths have no dot in their first path element
		first := p
		if i := strings.IndexByte(p, '/'); i >= 0 {
			first = p[:i]
		}
		if strings.Contains(first, ".") && !strings.HasPrefix(p, "serverwatch/") {
			t.Errorf("default build pulled in third-party package: %s", p)
		}
	}
}
