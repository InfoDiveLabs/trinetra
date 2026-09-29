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

// writeBundle writes a signed release bundle directory to disk (manifest,
// both signatures, and the given asset files side by side), the on-disk
// shape update.DirSource reads -- unlike signedRelease/mapSource, which only
// exist in memory and can't back a real --bundle DIR.
func writeBundle(t *testing.T, version string, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	m := update.Manifest{Schema: 1, Product: "trinetra", Version: version, Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.4.1"}
	for name, b := range files {
		m.Files = append(m.Files, mf(name, b))
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeFile := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("manifest.json", mb)
	writeFile("manifest.ci.sig", update.NewTestSigner(1).SignRelease(mb))
	writeFile("manifest.maint.sig", update.NewTestSigner(2).SignRelease(mb))
	return dir
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

// TestUpdaterApplyBundleWorksWhenChannelOff pins Ruling R7: an operator with
// update.channel=off (updates disabled from the network) must still be able
// to install an explicit local bundle via --bundle DIR. Policy.Channel comes
// from the verified bundle manifest's own channel in that case, not the
// host's "off" -- CheckPolicy would otherwise always refuse with
// ErrWrongChannel. Floor/min_upgrade_from/signature checks are unaffected
// (this bundle's version and MinUpgradeFrom are set up to pass them).
func TestUpdaterApplyBundleWorksWhenChannelOff(t *testing.T) {
	p := testUpdatePaths(t)
	bundleDir := writeBundle(t, "0.5.0", map[string][]byte{
		"trinetra-linux-amd64":     []byte("NEW-core"),
		"trinetra-web-linux-amd64": []byte("NEW-web"),
	})
	guarded := 0
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) },
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { guarded++; return nil }}
	c := config.Default()
	if err := c.Set("update.channel", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.apply(context.Background(), c, applyOptions{Bundle: bundleDir}); err != nil {
		t.Fatal(err)
	}
	if guarded != 1 {
		t.Fatalf("guard launched %d times", guarded)
	}
}

// TestUpdaterApplyRefusesChannelOffWithoutBundle is R7's other half: without
// --bundle, update.channel=off must still refuse -- "off" only ever yields
// to an explicit local bundle, never to the network source.
func TestUpdaterApplyRefusesChannelOffWithoutBundle(t *testing.T) {
	p := testUpdatePaths(t)
	u := updater{paths: p, keys: testKeys(), running: mustVer("0.4.1"), launchGuard: func() error { return nil }}
	c := config.Default()
	if err := c.Set("update.channel", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.apply(context.Background(), c, applyOptions{}); err == nil {
		t.Fatal("apply with update.channel=off and no --bundle should be refused")
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

// TestUpdaterRollbackNormalizesVersion is fix-round-1 F1: `version --json`
// against the previous binary can report a "v"-prefixed version (git
// describe-style tags, e.g. "v0.4.1" -- see internal/version's Makefile
// stamping), and rollback() used to store that raw string as
// Pending.Version verbatim. The guard's health check compares Pending.Version
// against the RUNNING daemon's reported version (also normalised), so an
// un-normalised Pending.Version made every `trinetra update rollback`
// misreport a version mismatch and roll back a perfectly healthy restart.
func TestUpdaterRollbackNormalizesVersion(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	guarded := 0
	u := updater{paths: p, x: fakeVersionExec("v0.4.1"), now: func() time.Time { return time.Unix(1000, 0) },
		running: mustVer("0.5.0"), launchGuard: func() error { guarded++; return nil }}
	if err := u.rollback(); err != nil {
		t.Fatal(err)
	}
	if guarded != 1 {
		t.Fatalf("guard launched %d times", guarded)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending == nil || st.Pending.Version != "0.4.1" {
		t.Fatalf("pending version not normalised: %+v", st.Pending)
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
