package trinetra

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

func TestVerifyInstallBundle(t *testing.T) {
	dir := t.TempDir()
	core := []byte("CORE")
	os.WriteFile(filepath.Join(dir, "trinetra"), core, 0o755)
	m := update.Manifest{Schema: 1, Product: "trinetra", Version: "0.5.0", Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.4.1",
		Files: []update.File{mf("trinetra-linux-"+runtime.GOARCH, core)}}
	m.Files[0].Arch = runtime.GOARCH
	mb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), updatetest.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), updatetest.NewTestSigner(2).SignRelease(mb), 0o644)

	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err != nil {
		t.Fatalf("valid bundle refused: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "trinetra"), []byte("EVIL"), 0o755)
	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err == nil || !strings.Contains(err.Error(), "does not match the signed manifest") {
		t.Fatalf("tampered binary: %v", err)
	}
	os.Remove(filepath.Join(dir, "manifest.maint.sig"))
	if _, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"}); err == nil {
		t.Fatal("missing maintainer sig accepted")
	}
}

// TestVerifyInstallBundleNoManifest pins errNoSignedManifest: absent
// manifest.json must be distinguishable from a present-but-invalid one, so
// cmdInstall (verifyInstallSignature) can warn-and-continue instead of
// refusing outright unless --require-signed was passed.
func TestVerifyInstallBundleNoManifest(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "trinetra")
	os.WriteFile(self, []byte("CORE"), 0o755)

	_, err := verifyInstallBundle(testKeys(), self, []string{"trinetra"})
	if err != errNoSignedManifest {
		t.Fatalf("err = %v, want errNoSignedManifest", err)
	}
}

// TestInstallAcceptsSignedPrereleaseZeroOnFreshHost pins the real-world
// rehearsal bug: a fresh host with nothing installed (running is the zero
// Version) and no persisted floor (a brand-new state dir, no state.json at
// all) must accept a signed 0.0.0-rc.1 release. Before the fix, "no floor"
// was represented as the zero Version itself, and 0.0.0-rc.1 sorts below
// 0.0.0 by semver precedence, so the install was wrongly refused as a
// downgrade.
func TestInstallAcceptsSignedPrereleaseZeroOnFreshHost(t *testing.T) {
	dir := t.TempDir()
	core := []byte("CORE")
	os.WriteFile(filepath.Join(dir, "trinetra"), core, 0o755)
	// Channel beta: a pre-release version must not be on the stable channel
	// (manifest.go's own decode rule) -- unrelated to the floor bug this test
	// pins, so pick a channel that lets a pre-release version through at all.
	m := update.Manifest{Schema: 1, Product: "trinetra", Version: "0.0.0-rc.1", Channel: "beta",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: "0.0.0",
		Files: []update.File{mf("trinetra-linux-"+runtime.GOARCH, core)}}
	m.Files[0].Arch = runtime.GOARCH
	mb, _ := json.Marshal(m)
	os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.ci.sig"), updatetest.NewTestSigner(1).SignRelease(mb), 0o644)
	os.WriteFile(filepath.Join(dir, "manifest.maint.sig"), updatetest.NewTestSigner(2).SignRelease(mb), 0o644)

	verified, err := verifyInstallBundle(testKeys(), filepath.Join(dir, "trinetra"), []string{"trinetra"})
	if err != nil {
		t.Fatalf("signed 0.0.0-rc.1 bundle refused: %v", err)
	}

	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fresh state dir: no state.json at all (no persisted floor), and running
	// is the zero Version (nothing installed yet).
	if err := checkInstallPolicy(paths, verified, update.Version{}); err != nil {
		t.Fatalf("checkInstallPolicy(0.0.0-rc.1, no floor, fresh host) refused: %v", err)
	}
}

// freshInstallManifest returns a minimal valid manifest for
// checkInstallPolicy/raiseInstallFloor tests -- these exercise state
// plumbing, not signature verification (verifyInstallBundle already covers
// that), so the manifest itself never needs to be signed here.
func freshInstallManifest(version, minUpgradeFrom string) update.Manifest {
	return update.Manifest{Schema: 1, Product: "trinetra", Version: version, Channel: "stable",
		Published: "2026-10-01T10:00:00Z", MinUpgradeFrom: minUpgradeFrom}
}

// TestCheckInstallPolicyUsesUpdateStateDir pins fix round 1's Ruling R8: the
// install floor check must read the SAME state.json `trinetra update`/the
// guard/status already share (defaultUpdatePaths().dir(), i.e.
// StateDir/update) -- not the bare state directory. A floor seeded directly
// at paths.dir() must be honored: a release below it is refused, one above
// it succeeds.
func TestCheckInstallPolicyUsesUpdateStateDir(t *testing.T) {
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := update.SaveState(paths.dir(), update.State{Floor: "0.6.0"}); err != nil {
		t.Fatalf("seed floor: %v", err)
	}

	// Below the persisted floor: refused, even though running (a fresh/
	// unknown host) is the zero Version -- the floor check is independent of
	// Running.
	below := freshInstallManifest("0.5.0", "0.4.1")
	if err := checkInstallPolicy(paths, below, update.Version{}); err == nil || !errors.Is(err, update.ErrDowngrade) {
		t.Fatalf("checkInstallPolicy(0.5.0, floor 0.6.0) = %v, want a wrapped update.ErrDowngrade", err)
	}

	// Above the persisted floor: succeeds.
	above := freshInstallManifest("0.7.0", "0.4.1")
	if err := checkInstallPolicy(paths, above, update.Version{}); err != nil {
		t.Fatalf("checkInstallPolicy(0.7.0, floor 0.6.0) refused: %v", err)
	}
}

// TestCheckInstallPolicyUnparsableFloorFailsClosed is #138 truth-table case
// (d)'s install half: a persisted floor that is valid JSON but not a valid
// version must refuse the install with a clear error, never silently act as
// "no floor" (which, with an unknown running version on a fresh host, would
// enforce no lower bound at all).
func TestCheckInstallPolicyUnparsableFloorFailsClosed(t *testing.T) {
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := update.SaveState(paths.dir(), update.State{Floor: "not-a-version"}); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	m := freshInstallManifest("0.7.0", "0.4.1")
	if err := checkInstallPolicy(paths, m, update.Version{}); err == nil || !strings.Contains(err.Error(), "not-a-version") {
		t.Fatalf("checkInstallPolicy() = %v, want an error naming the bad floor value", err)
	}
}

// TestRaiseInstallFloorWritesOnlyUnderUpdateStateDir pins the other half of
// Ruling R8: raiseInstallFloor must write state.json under paths.dir()
// (StateDir/update), never directly in StateDir -- and must never touch
// StateDir's own permissions (update.SaveState chmods the directory IT is
// given to 0700; passing the bare StateDir previously re-permissioned
// /var/lib/trinetra itself, which normally stays 0755).
func TestRaiseInstallFloorWritesOnlyUnderUpdateStateDir(t *testing.T) {
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	raiseInstallFloor(paths, "0.5.0")

	st, err := update.LoadState(paths.dir())
	if err != nil {
		t.Fatalf("LoadState(paths.dir()): %v", err)
	}
	if st.Floor != "0.5.0" {
		t.Errorf("floor = %q, want 0.5.0 (raiseInstallFloor must write to paths.dir(), not the bare state dir)", st.Floor)
	}
	if _, err := os.Stat(filepath.Join(paths.StateDir, "state.json")); err == nil {
		t.Error("state.json was written directly in StateDir; it must only live under StateDir/update")
	}
	fi, err := os.Stat(paths.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o755 {
		t.Errorf("StateDir permissions = %o, want unchanged 0755 (raiseInstallFloor must never chmod the bare state dir)", perm)
	}
}

// TestCheckInstallPolicySkipsMinUpgradeFromWhenRunningUnknown pins fix round
// 1's Ruling R9: when running is the zero Version (a fresh host, or a
// serverwatch migration with nothing at /usr/local/bin/trinetra yet), a
// signed install must not be refused just because the manifest's
// MinUpgradeFrom is above 0.0.0 -- only the floor (still zero/unset here)
// and the caller's already-run signature/hash checks gate it.
func TestCheckInstallPolicySkipsMinUpgradeFromWhenRunningUnknown(t *testing.T) {
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := freshInstallManifest("1.0.0", "0.9.0")
	if err := checkInstallPolicy(paths, m, update.Version{}); err != nil {
		t.Fatalf("fresh-host install of 1.0.0 (min_upgrade_from 0.9.0) refused: %v", err)
	}
}

// TestCheckInstallPolicyEnforcesMinUpgradeFromWhenRunningKnown is the flip
// side: once a real running version IS known, MinUpgradeFrom is still
// enforced exactly as update.CheckPolicy always has -- Ruling R9 only skips
// the check when running is genuinely unknown, it does not disable it.
func TestCheckInstallPolicyEnforcesMinUpgradeFromWhenRunningKnown(t *testing.T) {
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m := freshInstallManifest("1.0.0", "0.9.0")
	tooOld := update.Version{Major: 0, Minor: 5, Patch: 0}
	if err := checkInstallPolicy(paths, m, tooOld); err == nil || !errors.Is(err, update.ErrTooOld) {
		t.Fatalf("checkInstallPolicy(running 0.5.0, min_upgrade_from 0.9.0) = %v, want a wrapped update.ErrTooOld", err)
	}

	newEnough := update.Version{Major: 0, Minor: 9, Patch: 5}
	if err := checkInstallPolicy(paths, m, newEnough); err != nil {
		t.Fatalf("checkInstallPolicy(running 0.9.5, min_upgrade_from 0.9.0) refused: %v", err)
	}
}
