package trinetra

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

type fakeHealth struct {
	active  bool
	version string
	ts      int64
}

func (f *fakeHealth) Active() bool { return f.active }
func (f *fakeHealth) Version() (string, error) {
	if f.version == "" {
		return "", errors.New("down")
	}
	return f.version, nil
}
func (f *fakeHealth) SampleTS() (int64, error) { return f.ts, nil }

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func guardFixture(t *testing.T, pending update.Pending) (updatePaths, *fakeClock) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("NEW-core"), 0o755)
	update.SaveState(p.dir(), update.State{Floor: "0.4.1", Pending: &pending})
	return p, &fakeClock{t: time.Unix(1000, 0)}
}

func TestGuardCommitsWhenHealthy(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "v0.5.0", ts: 1002}
	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err != nil || r.Outcome != "committed" || restarts != 1 {
		t.Fatalf("r=%+v err=%v restarts=%d", r, err, restarts)
	}
	st, _ := update.LoadState(p.dir())
	if st.Floor != "0.5.0" || st.Pending != nil {
		t.Fatalf("state %+v", st)
	}
}

func TestGuardRollsBackOnEachFailedCondition(t *testing.T) {
	for name, h := range map[string]*fakeHealth{
		"inactive":     {active: false, version: "0.5.0", ts: 1002},
		"old version":  {active: true, version: "0.4.1", ts: 1002},
		"stale sample": {active: true, version: "0.5.0", ts: 999},
		"socket down":  {active: true, version: "", ts: 1002},
	} {
		p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
		r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
		if err != nil || r.Outcome != "rolled_back" {
			t.Errorf("%s: r=%+v err=%v", name, r, err)
			continue
		}
		b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra"))
		st, _ := update.LoadState(p.dir())
		if string(b) != "OLD-core" || st.Floor != "0.4.1" || !st.IsBad("0.5.0") || st.Pending != nil {
			t.Errorf("%s: bin=%q state=%+v", name, b, st)
		}
	}
}

// TestGuardNormalizesVPrefixedPendingVersion: Pending.Version can carry a "v" prefix.
func TestGuardNormalizesVPrefixedPendingVersion(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "v0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "0.5.0", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

// TestGuardRollbackCommitDoesNotRaiseFloorOrMarkBad: a healthy rollback confirmation must
// commit without raising the floor.
func TestGuardRollbackCommitDoesNotRaiseFloorOrMarkBad(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	// rollback() already restores previous/ into BinDir before launching the guard (see
	// updater.rollback).
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("OLD-core"), 0o755)
	pending := update.Pending{Version: "0.4.1", From: "0.5.0", Deadline: 1090, Files: []string{"trinetra"}, Rollback: true}
	if err := update.SaveState(p.dir(), update.State{Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := &fakeHealth{active: true, version: "0.4.1", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	st, _ := update.LoadState(p.dir())
	if st.Floor != "" {
		t.Fatalf("healthy rollback raised the floor: %+v", st)
	}
	if st.IsBad("0.4.1") {
		t.Fatalf("healthy rollback marked a version bad: %+v", st)
	}
	if st.Pending != nil {
		t.Fatalf("pending not cleared: %+v", st)
	}
}

// TestGuardRollbackFailedGateDoesNotLoop: when a rollback confirmation never becomes
// healthy there is no older build to fall back to (previous/ already IS the running build).
func TestGuardRollbackFailedGateDoesNotLoop(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("OLD-core"), 0o755)
	pending := update.Pending{Version: "0.4.1", From: "0.5.0", Deadline: 1090, Files: []string{"trinetra"}, Rollback: true}
	if err := update.SaveState(p.dir(), update.State{Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := &fakeHealth{active: false, version: "0.4.1", ts: 1002} // never healthy
	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err != nil || r.Outcome != "rolled_back" || r.Detail == "" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if restarts != 1 {
		t.Fatalf("restarts=%d, want 1 (a failed rollback gate must not loop/re-restart)", restarts)
	}
	b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra"))
	if string(b) != "OLD-core" {
		t.Fatalf("binaries changed on a failed rollback gate: %q", b)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending != nil {
		t.Fatalf("pending not cleared: %+v", st)
	}
	if st.IsBad("0.4.1") {
		t.Fatalf("failed rollback gate marked a version bad: %+v", st)
	}
	if st.Floor != "" {
		t.Fatalf("failed rollback gate raised the floor: %+v", st)
	}
}

// TestGuardKeepsPendingWhenRestoreFails is issue #136: when a Pending fails its health gate
// AND restoring the previous build itself then fails (e.g. a write error in BinDir).
func TestGuardKeepsPendingWhenRestoreFails(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}, Phase: pendingSwapped})
	h := &fakeHealth{active: false, version: "0.5.0", ts: 1002} // never healthy -> rolls back
	chmodUnwritable(t, p.BinDir)                                // restorePrevious can't write into BinDir

	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err == nil {
		t.Fatal("runGuard succeeded despite a failed restore")
	}
	if !strings.Contains(err.Error(), "watchdog will retry") {
		t.Fatalf("error does not say the watchdog will retry: %v", err)
	}
	if r.Outcome != "" {
		t.Fatalf("r=%+v, want a zero Result while the restore keeps failing", r)
	}
	if restarts != 2 {
		t.Fatalf("restarts=%d, want 2 (the initial restart onto the pending build, plus the rollback's own restart attempt)", restarts)
	}

	st, err := update.LoadState(p.dir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil {
		t.Fatal("Pending cleared despite a failed restore; the watchdog will never retry (#136)")
	}
	if st.Pending.Version != "0.5.0" || st.Pending.From != "0.4.1" {
		t.Fatalf("Pending mangled: %+v", st.Pending)
	}
	if st.Pending.RestoreFailed == "" {
		t.Fatal("Pending.RestoreFailed not recorded")
	}
	if !strings.Contains(st.Pending.RestoreFailed, "restoring the previous build also failed") {
		t.Fatalf("Pending.RestoreFailed = %q, missing the restore failure", st.Pending.RestoreFailed)
	}
	if st.IsBad("0.5.0") {
		t.Fatal("version marked bad before the restore ever actually succeeded")
	}
	if st.Floor != "0.4.1" {
		t.Fatalf("floor changed: %+v", st)
	}
	if st.Last != nil {
		t.Fatalf("Last recorded before the rollback actually finished: %+v", st.Last)
	}

	// Second guard run: fix the write error.
	if err := os.Chmod(p.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r2, err2 := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err2 != nil || r2.Outcome != "rolled_back" {
		t.Fatalf("retry: r=%+v err=%v", r2, err2)
	}
	b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra"))
	if string(b) != "OLD-core" {
		t.Fatalf("bin not restored after the retry: %q", b)
	}
	st2, err := update.LoadState(p.dir())
	if err != nil {
		t.Fatal(err)
	}
	if st2.Pending != nil {
		t.Fatalf("Pending not cleared after a successful retry: %+v", st2.Pending)
	}
	if !st2.IsBad("0.5.0") {
		t.Fatal("version not marked bad once the retry finally rolled back")
	}
	if st2.Last == nil || st2.Last.Outcome != "rolled_back" {
		t.Fatalf("Last not recorded as rolled_back: %+v", st2.Last)
	}
}

// TestGuardRetryDoesNotReRunHealthGate pins #136: once a Pending has already failed its
// health gate AND a restore attempt, the rollback decision is final.
func TestGuardRetryDoesNotReRunHealthGate(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{
		Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}, Phase: pendingSwapped,
		RestoreFailed:       "trinetra.service is not active; restoring the previous build also failed: permission denied",
		RestoreFailedReason: "trinetra.service is not active",
	})
	// Never consulted by a correct guard -- if it were, this reports
	// healthy and a buggy guard would commit instead of rolling back.
	h := &fakeHealth{active: true, version: "0.5.0", ts: 2000}
	chmodUnwritable(t, p.BinDir)

	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err == nil {
		t.Fatal("runGuard succeeded despite a still-failing restore")
	}
	if r.Outcome != "" {
		t.Fatalf("r=%+v, want a zero Result while the restore keeps failing", r)
	}
	if restarts != 0 {
		t.Fatalf("restarts=%d, want 0: a retry-only run must not restart the still-pending, already-condemned build", restarts)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending == nil || st.Pending.RestoreFailedReason != "trinetra.service is not active" {
		t.Fatalf("RestoreFailedReason lost or Pending cleared: %+v", st.Pending)
	}

	// Fix the write error and retry: even though the (never-consulted) health fake would
	// report healthy, the guard must still roll back -- not commit.
	if err := os.Chmod(p.BinDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r2, err2 := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err2 != nil || r2.Outcome != "rolled_back" {
		t.Fatalf("r=%+v err=%v, want a rollback even though health would now pass", r2, err2)
	}
	b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra"))
	if string(b) != "OLD-core" {
		t.Fatalf("bin not restored: %q", b)
	}
	st2, _ := update.LoadState(p.dir())
	if st2.Pending != nil {
		t.Fatalf("Pending not cleared: %+v", st2.Pending)
	}
	if !st2.IsBad("0.5.0") {
		t.Fatal("version not marked bad")
	}
	if st2.Floor != "0.4.1" {
		t.Fatalf("floor raised despite a rollback: %+v", st2)
	}
	if restarts != 1 {
		t.Fatalf("restarts=%d, want 1 (only once the retry's restore actually succeeded)", restarts)
	}
	if st2.Last == nil || st2.Last.Detail != "trinetra.service is not active" {
		t.Fatalf("Last.Detail = %+v, want the original health-gate reason, not accumulated restore-retry noise", st2.Last)
	}
}

// TestGuardRestoreFailureRollbackDoesNotAffectRollbackConfirmation is a guard-rail
// alongside TestGuardKeepsPendingWhenRestoreFails: a rollback confirmation.
func TestGuardRestoreFailureRollbackDoesNotAffectRollbackConfirmation(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("OLD-core"), 0o755)
	pending := update.Pending{Version: "0.4.1", From: "0.5.0", Deadline: 1090, Files: []string{"trinetra"}, Rollback: true}
	if err := update.SaveState(p.dir(), update.State{Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	chmodUnwritable(t, p.BinDir)
	clk := &fakeClock{t: time.Unix(1000, 0)}
	h := &fakeHealth{active: false, version: "0.4.1", ts: 1002} // never healthy
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "rolled_back" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending != nil {
		t.Fatalf("pending not cleared: %+v", st)
	}
}

// TestGuardCommitsDespiteRestartError pins that runGuard does not treat restart()'s error
// as fatal: osExec's 60s execTimeout is shorter than systemd's default 90s TimeoutStopSec.
func TestGuardCommitsDespiteRestartError(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "0.5.0", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep,
		restart: func() error { return errors.New("systemctl restart: context deadline exceeded") }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

// TestGuardRollsBackWhenRestartErrorsAndNeverHealthy: a restart() error that reflects a
// real problem (the daemon never comes up healthy) must still roll back by the deadline.
func TestGuardRollsBackWhenRestartErrorsAndNeverHealthy(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: false, version: "0.5.0", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep,
		restart: func() error { return errors.New("systemctl restart: context deadline exceeded") }})
	if err != nil || r.Outcome != "rolled_back" || r.Detail == "" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	st, _ := update.LoadState(p.dir())
	if !st.IsBad("0.5.0") || st.Pending != nil {
		t.Fatalf("state %+v", st)
	}
}
