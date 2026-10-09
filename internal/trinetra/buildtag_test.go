package trinetra

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultBuildIsStdlibOnly asserts the DEFAULT build of the trinetra DAEMON BINARY
// (cmd/trinetra) imports no third-party packages.
func TestDefaultBuildIsStdlibOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go list in short mode")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "./cmd/trinetra")
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
		if strings.Contains(first, ".") && !strings.HasPrefix(p, "github.com/InfoDiveLabs/trinetra/") {
			t.Errorf("default build pulled in third-party package: %s", p)
		}
	}
}
