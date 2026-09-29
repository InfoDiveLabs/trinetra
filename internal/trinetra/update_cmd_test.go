package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

func signedRelease(t *testing.T, version string, files map[string][]byte) mapSource {
	t.Helper()
	m := update.Manifest{Schema: 1, Product: "trinetra", Version: version, Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.4.1"}
	src := mapSource{}
	for name, b := range files {
		m.Files = append(m.Files, mf(name, b))
		src[name] = b
	}
	mb, _ := json.Marshal(m)
	src["manifest.json"] = mb
	src["manifest.ci.sig"] = update.NewTestSigner(1).SignRelease(mb)
	src["manifest.maint.sig"] = update.NewTestSigner(2).SignRelease(mb)
	return src
}

func testKeys() update.KeySet {
	return update.KeySet{CI: []update.PublicKey{update.NewTestSigner(1).Public()}, Maint: []update.PublicKey{update.NewTestSigner(2).Public()}, Pointer: []update.PublicKey{update.NewTestSigner(3).Public()}}
}

// NOTE (deviation from task-6-brief.md): testUpdatePaths (update_apply_test.go)
// pre-populates BinDir with BOTH "trinetra" and "trinetra-web" as "installed"
// binaries. planApply (task 5, already committed) requires a release asset
// for every binary planApply's `installed` closure reports present -- so a
// release that only ships trinetra-linux-amd64 is rejected once trinetra-web
// is also "installed". The brief's TestUpdaterApplyLaunchesGuardAndRefusesDowngrade
// only supplied a core asset; that combination cannot pass planApply given
// testUpdatePaths' fixture, so trinetra-web-linux-amd64 is added to the
// release here to keep reusing the existing fixture rather than forking it.
func TestUpdaterApplyLaunchesGuardAndRefusesDowngrade(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.5.0", map[string][]byte{
		"trinetra-linux-amd64":     []byte("NEW-core"),
		"trinetra-web-linux-amd64": []byte("NEW-web"),
	})
	guarded := 0
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) },
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { guarded++; return nil }, src: src}
	c := config.Default()
	if _, err := u.apply(context.Background(), c, applyOptions{Version: "0.5.0"}); err != nil {
		t.Fatal(err)
	}
	if guarded != 1 {
		t.Fatalf("guard launched %d times", guarded)
	}
	st, _ := update.LoadState(p.dir())
	st.Pending = nil
	st.Floor = "0.6.0"
	update.SaveState(p.dir(), st)
	if _, err := u.apply(context.Background(), c, applyOptions{Version: "0.5.0"}); !errors.Is(err, update.ErrDowngrade) {
		t.Fatalf("downgrade: %v", err)
	}
}

func TestUpdaterApplyRefusesBadVersionWithoutForce(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.5.0", map[string][]byte{"trinetra-linux-amd64": []byte("NEW-core")})
	update.SaveState(p.dir(), update.State{Bad: []string{"0.5.0"}})
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: time.Now, arch: "amd64",
		running: mustVer("0.4.1"), launchGuard: func() error { return nil }, src: src}
	if _, err := u.apply(context.Background(), config.Default(), applyOptions{Version: "0.5.0"}); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("bad version applied without --force: %v", err)
	}
}

// TestConfigGetRedactsSecrets adapts task-6-brief.md's version (which called
// a `run([]string{...})` helper and `withStdout`/`withTempConfig` that don't
// exist in this codebase) to the real entry point: Main(), and the
// cfgPath/stdout package vars main_test.go already overrides directly (see
// TestConfigSetGetViaCLI).
func TestConfigGetRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")

	c, err := loadCfg()
	if err != nil {
		t.Fatal(err)
	}
	c.Update.GitHubToken = "ghp_SECRET"
	if err := saveCfg(c); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	stdout = &out
	Main([]string{"config", "get"})
	Main([]string{"config", "get", "update.github_token"})
	if strings.Contains(out.String(), "ghp_SECRET") || !strings.Contains(out.String(), "(set)") {
		t.Fatalf("secret leaked or not marked:\n%s", out.String())
	}
}

type fakeVersionExec string

func (f fakeVersionExec) Run(name string, args ...string) ([]byte, error) {
	return []byte(`{"version":"` + string(f) + `"}`), nil
}

func mustVer(s string) update.Version { v, _ := update.ParseVersion(s); return v }

// TestUpdateStatusJSONWorksWithNoStateDir pins Ruling R2
// (.superpowers/sdd/2026-09-29-signed-releases-self-update/progress.md):
// `trinetra update status --json` must succeed for a non-root caller with
// no /var/lib/trinetra present at all (a fresh CI runner before any update
// has ever run), and the JSON must include a "fingerprints" field sourced
// from update.Fingerprints(update.ProductionKeys()).
func TestUpdateStatusJSONWorksWithNoStateDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	prevStateDir := stateDir
	stateDir = filepath.Join(dir, "does-not-exist-yet")
	t.Cleanup(func() { stateDir = prevStateDir })

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"update", "status", "--json"}); code != 0 {
		t.Fatalf("update status --json exit=%d, output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), `"fingerprints"`) {
		t.Fatalf("status --json missing fingerprints field:\n%s", out.String())
	}
	var st updateStatus
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, out.String())
	}
	wantFP := update.Fingerprints(update.ProductionKeys())
	if len(st.Fingerprints) != len(wantFP) {
		t.Fatalf("fingerprints = %v, want %v", st.Fingerprints, wantFP)
	}
}

// TestRunGuardCommitsHealthyPending pins the guard's forward-update happy
// path: a pending update whose deadline has already passed, a core binary
// that smoke-tests as healthy, commits -- the floor is raised to the
// pending version, Pending is cleared, and Last records "committed".
func TestRunGuardCommitsHealthyPending(t *testing.T) {
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{Pending: &update.Pending{
		Version: "0.5.0", From: "0.4.1", Deadline: 1, Files: []string{"trinetra"},
	}})
	u := updater{paths: p, x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) }, running: mustVer("0.4.1")}

	if code := runGuard(u); code != 0 {
		t.Fatalf("runGuard exit=%d", code)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending != nil {
		t.Fatalf("pending not cleared: %+v", st.Pending)
	}
	if st.Floor != "0.5.0" {
		t.Fatalf("floor = %q, want 0.5.0", st.Floor)
	}
	if st.Last == nil || st.Last.Outcome != "committed" {
		t.Fatalf("last = %+v, want committed", st.Last)
	}
}

// TestRunGuardRollsBackUnhealthyPending pins the guard's failure path: a
// pending update whose core binary fails the smoke test (wrong version)
// gets rolled back, the floor is NOT raised, and Last records
// "rolled_back".
func TestRunGuardRollsBackUnhealthyPending(t *testing.T) {
	p := testUpdatePaths(t)
	if err := os.MkdirAll(p.previous(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	update.SaveState(p.dir(), update.State{Pending: &update.Pending{
		Version: "0.5.0", From: "0.4.1", Deadline: 1, Files: []string{"trinetra"},
	}})
	// fakeVersionExec always reports "0.3.0", which never matches the
	// pending version 0.5.0 -> smokeTest fails -> unhealthy.
	u := updater{paths: p, x: fakeVersionExec("0.3.0"), now: func() time.Time { return time.Unix(1000, 0) }, running: mustVer("0.4.1")}

	if code := runGuard(u); code != 0 {
		t.Fatalf("runGuard exit=%d", code)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending != nil {
		t.Fatalf("pending not cleared: %+v", st.Pending)
	}
	if st.Floor != "" {
		t.Fatalf("floor raised on a failed update: %q", st.Floor)
	}
	if st.Last == nil || st.Last.Outcome != "rolled_back" {
		t.Fatalf("last = %+v, want rolled_back", st.Last)
	}
	if b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra")); string(b) != "OLD-core" {
		t.Fatalf("core not restored: %q", b)
	}
}
