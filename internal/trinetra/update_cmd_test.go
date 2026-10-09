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
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
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
	src["manifest.ci.sig"] = updatetest.NewTestSigner(1).SignRelease(mb)
	src["manifest.maint.sig"] = updatetest.NewTestSigner(2).SignRelease(mb)
	return src
}

func testKeys() update.KeySet {
	return update.KeySet{CI: []update.PublicKey{updatetest.NewTestSigner(1).Public()}, Maint: []update.PublicKey{updatetest.NewTestSigner(2).Public()}, Pointer: []update.PublicKey{updatetest.NewTestSigner(3).Public()}}
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
	writeFile("manifest.ci.sig", updatetest.NewTestSigner(1).SignRelease(mb))
	writeFile("manifest.maint.sig", updatetest.NewTestSigner(2).SignRelease(mb))
	return dir
}

// NOTE: testUpdatePaths (update_apply_test.go) pre-populates BinDir with BOTH
// "trinetra" and "trinetra-web" as "installed" binaries, and planApply requires
// a release asset for every installed binary, so a release shipping only
// trinetra-linux-amd64 is rejected. trinetra-web-linux-amd64 is added to the
// release here to reuse the fixture rather than fork it.
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

// TestUpdaterApplyNoFloorFallsBackToRunning pins #138 truth-table case (b):
// with nothing persisted in state.json, FloorVersion falls back to the
// running version as the floor, so a pre-release of that same version
// (0.5.0-rc.1 sorts below 0.5.0 by semver precedence) is refused as a
// downgrade even though no floor was ever written.
func TestUpdaterApplyNoFloorFallsBackToRunning(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.5.0-rc.1", map[string][]byte{
		"trinetra-linux-amd64":     []byte("NEW-core"),
		"trinetra-web-linux-amd64": []byte("NEW-web"),
	})
	var m update.Manifest
	if err := json.Unmarshal(src["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	m.Channel = "beta" // a pre-release version must not be on the stable channel
	mb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	src["manifest.json"] = mb
	src["manifest.ci.sig"] = updatetest.NewTestSigner(1).SignRelease(mb)
	src["manifest.maint.sig"] = updatetest.NewTestSigner(2).SignRelease(mb)

	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0-rc.1"), now: time.Now,
		arch: "amd64", running: mustVer("0.5.0"), launchGuard: func() error { return nil }, src: src}
	c := config.Default()
	if err := c.Set("update.channel", "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := u.apply(context.Background(), c, applyOptions{Version: "0.5.0-rc.1"}); !errors.Is(err, update.ErrDowngrade) {
		t.Fatalf("apply(0.5.0-rc.1, no floor, running 0.5.0) = %v, want update.ErrDowngrade", err)
	}
}

// TestUpdaterApplyUnparsableFloorFailsClosed is #138 truth-table case (d)'s
// apply half: a persisted floor that is valid JSON but not a valid version
// must refuse `trinetra update apply` with a clear error, never silently act
// as "no floor".
func TestUpdaterApplyUnparsableFloorFailsClosed(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.7.0", map[string][]byte{
		"trinetra-linux-amd64":     []byte("NEW-core"),
		"trinetra-web-linux-amd64": []byte("NEW-web"),
	})
	if err := update.SaveState(p.dir(), update.State{Floor: "not-a-version"}); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.7.0"), now: time.Now,
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { return nil }, src: src}
	if _, err := u.apply(context.Background(), config.Default(), applyOptions{Version: "0.7.0"}); err == nil || !strings.Contains(err.Error(), "not-a-version") {
		t.Fatalf("apply() = %v, want an error naming the bad floor value", err)
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

// TestUpdaterApplyBundleWorksWhenChannelOff: an operator with
// update.channel=off (updates disabled from the network) must still be able to
// install an explicit local bundle via --bundle DIR. Policy.Channel comes from
// the verified bundle manifest's own channel in that case, not the host's
// "off", which CheckPolicy would otherwise refuse with ErrWrongChannel.
// Floor/min_upgrade_from/signature checks are unaffected.
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

// TestUpdaterApplyRefusesChannelOffWithoutBundle: without --bundle,
// update.channel=off must still refuse -- "off" only ever yields to an
// explicit local bundle, never to the network source.
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

// TestConfigGetRedactsSecrets drives the real entry point, Main(), with the
// cfgPath/stdout package vars main_test.go overrides directly (see
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

// TestUpdaterRollbackNormalizesVersion: `version --json` against the previous
// binary can report a "v"-prefixed version (git describe-style tags, e.g.
// "v0.4.1"), and rollback() must not store that raw string as Pending.Version.
// The guard compares Pending.Version against the RUNNING daemon's (normalised)
// version, so an un-normalised value would make every `trinetra update
// rollback` misreport a version mismatch and roll back a healthy restart.
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

// TestUpdateStatusJSONWorksWithNoStateDir: `trinetra update status --json`
// must succeed for a non-root caller with no /var/lib/trinetra at all (a fresh
// CI runner before any update has run), and the JSON must include a
// "fingerprints" field sourced from update.Fingerprints(update.ProductionKeys()).
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

// TestUpdaterStatusShowsUnparsableFloorInsteadOfCrashing is #138: `trinetra
// update status` must still display when the persisted floor is unparsable
// -- showing the error rather than refusing to run entirely, unlike
// check/apply/install which refuse the operation outright.
func TestUpdaterStatusShowsUnparsableFloorInsteadOfCrashing(t *testing.T) {
	p := testUpdatePaths(t)
	if err := update.SaveState(p.dir(), update.State{Floor: "not-a-version"}); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	u := updater{paths: p, running: mustVer("0.4.1")}
	st, err := u.status()
	if err != nil {
		t.Fatalf("status() = %v, want nil (must still display, not crash)", err)
	}
	if !strings.Contains(st.Floor, "not-a-version") {
		t.Fatalf("Floor = %q, want it to show the bad floor value", st.Floor)
	}
}

// A release recorded as available stops being offered once it is the
// running version, without waiting for the next channel check.
func TestStatusHidesAvailableOnceInstalled(t *testing.T) {
	p := testUpdatePaths(t)
	if err := update.SaveState(p.dir(), update.State{Available: "0.6.0-beta.1"}); err != nil {
		t.Fatal(err)
	}
	st, err := updater{paths: p, running: mustVer("0.6.0-beta.1")}.status()
	if err != nil {
		t.Fatal(err)
	}
	if st.Available != "" {
		t.Fatalf("Available = %q while running it", st.Available)
	}
	st, _ = updater{paths: p, running: mustVer("0.5.0")}.status()
	if st.Available != "0.6.0-beta.1" {
		t.Fatalf("Available = %q, want it offered to 0.5.0", st.Available)
	}
}
