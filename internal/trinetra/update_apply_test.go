package trinetra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	p := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state"),
		GuardDir: filepath.Join(root, "lib", "guard"), UnitDir: filepath.Join(root, "units")}
	os.MkdirAll(p.BinDir, 0o755)
	os.MkdirAll(p.StateDir, 0o755)
	os.MkdirAll(p.UnitDir, 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra-web"), []byte("OLD-web"), 0o755)

	prevStateDir := stateDir
	stateDir = p.StateDir
	t.Cleanup(func() { stateDir = prevStateDir })

	// The pinned guard is a copy of the running binary; point that at a
	// small fixture instead of the test binary.
	self := filepath.Join(root, "self")
	os.WriteFile(self, []byte("SELF-bin"), 0o755)
	prevSelf := selfExecutable
	selfExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfExecutable = prevSelf })

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

// --- fix round 1 ---

// TestTimeoutExecKillsSlowCommand pins F1: smokeTest must be bounded by its
// own fixed timeout, not the shared 60s execTimeout. timeoutExec is what
// gives it that bound; a command that outlives the timeout must be killed
// (and Run must return an error) well before the command would finish on
// its own.
func TestTimeoutExecKillsSlowCommand(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	start := time.Now()
	_, err := timeoutExec{200 * time.Millisecond}.Run("sleep", "5")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("timeoutExec.Run did not error on a command that outlived its timeout")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("timeoutExec.Run took %v to return, want well under the 5s sleep", elapsed)
	}
}

// TestSmokeTest pins smokeTest's own logic (version match, mismatch,
// non-JSON output, exec failure) independent of any real timeout, using the
// package's existing fakeExec (docker_test.go) fake.
func TestSmokeTest(t *testing.T) {
	cases := []struct {
		name    string
		x       Exec
		want    string
		wantErr bool
	}{
		{
			name:    "version matches, v-prefix stripped both sides",
			x:       fakeExec{fn: func(string, ...string) ([]byte, error) { return []byte(`{"version":"0.5.0"}`), nil }},
			want:    "v0.5.0",
			wantErr: false,
		},
		{
			name:    "version mismatch",
			x:       fakeExec{fn: func(string, ...string) ([]byte, error) { return []byte(`{"version":"0.4.0"}`), nil }},
			want:    "0.5.0",
			wantErr: true,
		},
		{
			name:    "non-JSON output",
			x:       fakeExec{fn: func(string, ...string) ([]byte, error) { return []byte("not json"), nil }},
			want:    "0.5.0",
			wantErr: true,
		},
		{
			name:    "exec error",
			x:       fakeExec{fn: func(string, ...string) ([]byte, error) { return nil, errors.New("boom") }},
			want:    "0.5.0",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := smokeTest(tc.x, "/path/to/trinetra", tc.want)
			if (err != nil) != tc.wantErr {
				t.Fatalf("smokeTest() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestSwapInJoinsRestoreFailureWithOriginalError pins F2: when a failure in
// swapIn's replace phase triggers restorePrevious, and restorePrevious ALSO
// fails, the error swapIn returns must surface both failures (not silently
// discard the restore error), with wording that makes clear the host may be
// left half-updated. Making BinDir unwritable after staging forces exactly
// this: the first replaceFile call fails (can't create the temp file), and
// the restorePrevious it triggers fails for the same reason (still can't
// write into BinDir).
func TestSwapInJoinsRestoreFailureWithOriginalError(t *testing.T) {
	p := testUpdatePaths(t)
	newCore, newWeb := []byte("NEW-core"), []byte("NEW-web")
	plan := applyPlan{Manifest: update.Manifest{Version: "0.5.0"}, Arch: "amd64",
		Files: []update.File{mf("trinetra-linux-amd64", newCore), mf("trinetra-web-linux-amd64", newWeb)},
		Names: []string{"trinetra", "trinetra-web"}}
	src := mapSource{"trinetra-linux-amd64": newCore, "trinetra-web-linux-amd64": newWeb}
	if err := stage(context.Background(), p, src, plan); err != nil {
		t.Fatal(err)
	}
	chmodUnwritable(t, p.BinDir)

	err := swapIn(p, plan, time.Unix(1, 0))
	if err == nil {
		t.Fatal("swapIn succeeded despite an unwritable BinDir")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("errors.Is(err, fs.ErrPermission) = false: %v", err)
	}
	if !strings.Contains(err.Error(), "update: install trinetra:") {
		t.Fatalf("original swap failure not present: %v", err)
	}
	if !strings.Contains(err.Error(), "restoring the previous build also failed") {
		t.Fatalf("restore failure not present: %v", err)
	}
	if !strings.Contains(err.Error(), "half-updated") {
		t.Fatalf("missing half-updated warning: %v", err)
	}
}
