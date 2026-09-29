package trinetra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

type mapSource map[string][]byte

func (m mapSource) ReleaseAsset(_ context.Context, _ string, name string) (io.ReadCloser, error) {
	b, ok := m[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m mapSource) ChannelAsset(context.Context, string) (io.ReadCloser, error) {
	return nil, update.ErrNoChannel
}

func mf(name string, b []byte) update.File {
	s := sha256.Sum256(b)
	return update.File{Name: name, OS: "linux", Arch: "amd64", Size: int64(len(b)), SHA256: hex.EncodeToString(s[:])}
}

// testUpdatePaths sets up a temp BinDir/StateDir with an "installed" core and
// web binary, and points the package-level stateDir at the fixture's
// StateDir (restored on cleanup) so writePluginManifest -- called by swapIn
// -- writes plugins.json inside the temp dir rather than the real
// /var/lib/trinetra.
func testUpdatePaths(t *testing.T) updatePaths {
	root := t.TempDir()
	p := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	os.MkdirAll(p.BinDir, 0o755)
	os.MkdirAll(p.StateDir, 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra-web"), []byte("OLD-web"), 0o755)

	prevStateDir := stateDir
	stateDir = p.StateDir
	t.Cleanup(func() { stateDir = prevStateDir })

	return p
}

func TestPlanApplyPicksInstalledForArch(t *testing.T) {
	m := update.Manifest{Version: "0.5.0", Files: []update.File{
		mf("trinetra-linux-amd64", []byte("a")), mf("trinetra-web-linux-amd64", []byte("b")),
		mf("trinetra-ctl-linux-amd64", []byte("c")), {Name: "trinetra-linux-arm64", OS: "linux", Arch: "arm64", Size: 1, SHA256: "00"},
	}}
	installed := func(n string) bool { return n == "trinetra" || n == "trinetra-web" }
	plan, err := planApply(m, nil, "amd64", installed)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Names) != 2 || plan.Names[0] != "trinetra" || plan.Names[1] != "trinetra-web" {
		t.Fatalf("names %v", plan.Names)
	}
	if _, err := planApply(update.Manifest{Files: m.Files[1:3]}, nil, "amd64", installed); err == nil {
		t.Fatal("plan without the core binary accepted")
	}
}

func TestSwapInAndRestorePrevious(t *testing.T) {
	p := testUpdatePaths(t)
	newCore, newWeb := []byte("NEW-core"), []byte("NEW-web")
	plan := applyPlan{Manifest: update.Manifest{Version: "0.5.0"}, Arch: "amd64",
		Files: []update.File{mf("trinetra-linux-amd64", newCore), mf("trinetra-web-linux-amd64", newWeb)},
		Names: []string{"trinetra", "trinetra-web"}}
	src := mapSource{"trinetra-linux-amd64": newCore, "trinetra-web-linux-amd64": newWeb}
	if err := stage(context.Background(), p, src, plan); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	if err := swapIn(p, plan, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra")); string(b) != "NEW-core" {
		t.Fatalf("core not swapped: %q", b)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending == nil || st.Pending.Version != "0.5.0" || st.Pending.Deadline != now.Add(90*time.Second).Unix() {
		t.Fatalf("pending %+v", st.Pending)
	}
	if err := swapIn(p, plan, now); !errors.Is(err, errUpdateInProgress) {
		t.Fatalf("second swap while pending: %v", err)
	}
	if err := restorePrevious(p); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra-web")); string(b) != "OLD-web" {
		t.Fatalf("web not restored: %q", b)
	}
}

func TestSwapInRefusesStagedFileChangedAfterVerify(t *testing.T) {
	p := testUpdatePaths(t)
	newCore := []byte("NEW-core")
	plan := applyPlan{Manifest: update.Manifest{Version: "0.5.0"}, Arch: "amd64",
		Files: []update.File{mf("trinetra-linux-amd64", newCore)}, Names: []string{"trinetra"}}
	if err := stage(context.Background(), p, mapSource{"trinetra-linux-amd64": newCore}, plan); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.staging("0.5.0"), "trinetra-linux-amd64"), []byte("EVIL-cor"), 0o755)
	if err := swapIn(p, plan, time.Unix(1, 0)); err == nil {
		t.Fatal("swapped a staged file that no longer matches the manifest")
	}
	if b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra")); string(b) != "OLD-core" {
		t.Fatalf("installed binary changed: %q", b)
	}
	if st, _ := update.LoadState(p.dir()); st.Pending != nil {
		t.Fatal("pending recorded for a refused swap")
	}
}
