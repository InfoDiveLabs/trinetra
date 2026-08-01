package serverwatch

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultBuildIsStdlibOnly asserts the DEFAULT (untagged) build of the
// serverwatch DAEMON BINARY (cmd/serverwatch) imports no third-party
// packages: the web build tag (see web_deps.go/daemon_web.go/
// daemon_noweb.go) must keep go-webauthn, x/crypto/autocert, etc. entirely
// out of the daemon's module graph unless built with `-tags web`. This is
// the guard that makes the seam's isolation promise checkable rather than
// aspirational.
//
// Scope is deliberately cmd/serverwatch's own dependency graph, not
// "./..." (the whole module): cmd/serverwatch-ctl is a separate binary that
// carries its own third-party dependency (Bubble Tea, for its interactive
// TUI -- see cmd/serverwatch-ctl/tui.go) and is never imported by
// cmd/serverwatch, so it must not trip this guard. What the guard actually
// promises -- "the daemon you `systemctl start` is stdlib-only by default"
// -- only concerns cmd/serverwatch's own graph; scoping to "./..." would
// conflate the two binaries and make the daemon's guarantee unverifiable
// without also freezing every plugin command to stdlib.
//
// `go test` runs this package's tests with cwd == this directory
// (internal/serverwatch), but "go list -deps ./cmd/serverwatch" must
// expand from the MODULE ROOT — so the command's working directory is
// explicitly set to the module root (two levels up from this file) rather
// than relying on the test binary's default cwd.
func TestDefaultBuildIsStdlibOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go list in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "./cmd/serverwatch")
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
