package trinetra

import (
	"errors"
	"os"
	"path/filepath"
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

// TestGuardNormalizesVPrefixedPendingVersion is fix-round-1 F1: Pending.Version
// can carry a "v" prefix (e.g. update.rollback stored `version --json`'s raw
// output before that call was normalised too -- see
// TestUpdaterRollbackNormalizesVersion in update_cmd_test.go), while a
// healthy daemon's control.Client.Version() reports without one (or vice
// versa). checkGuardHealth must trim "v" on BOTH sides before comparing, the
// same way smokeTest already does, or a perfectly healthy restart is
// misdiagnosed as a version mismatch and wrongly rolled back.
func TestGuardNormalizesVPrefixedPendingVersion(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "v0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "0.5.0", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

// TestGuardRollbackCommitDoesNotRaiseFloorOrMarkBad is fix-round-1's
// Rollback=true coverage: a healthy rollback confirmation must commit
// without ever raising the floor (there is no prior floor here to leave
// unchanged from -- Floor starts unset, so a buggy RaiseFloor call would be
// directly observable) and without marking the rolled-back-to version bad.
func TestGuardRollbackCommitDoesNotRaiseFloorOrMarkBad(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	// rollback() already restores previous/ into BinDir before launching the
	// guard (see updater.rollback), so BinDir already holds the rollback
	// target by the time the guard runs.
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

// TestGuardRollbackFailedGateDoesNotLoop is fix-round-1's Rollback=true
// failure coverage: when a rollback confirmation never becomes healthy,
// there is no older build to fall back to (previous/ already IS the build
// currently running), so the guard must not attempt another
// restorePrevious/restart and must not mark anything bad -- it just records
// the failure and leaves the binaries as they are.
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

// TestGuardCommitsDespiteRestartError is fix-round-1 F2: osExec's own
// timeout (60s, execTimeout) is shorter than systemd's default 90s
// TimeoutStopSec, so `systemctl restart trinetra` can genuinely return an
// error (the command's own wait timed out) even though systemd goes on to
// finish the restart successfully a few seconds later. runGuard must not
// treat restart()'s error as fatal -- it must still poll for health, and
// commit if the daemon does come up healthy within the deadline.
func TestGuardCommitsDespiteRestartError(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "0.5.0", ts: 1002}
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep,
		restart: func() error { return errors.New("systemctl restart: context deadline exceeded") }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
}

// TestGuardRollsBackWhenRestartErrorsAndNeverHealthy is F2's other half: a
// restart() error that turns out to reflect a real problem (the daemon
// never comes up healthy) must still roll back by the deadline, exactly
// like any other failed health gate -- not hang or leave Pending set.
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

func TestResumePendingOnStartLaunchesGuardOnce(t *testing.T) {
	p, _ := guardFixture(t, update.Pending{Version: "0.5.0", Deadline: 1090})
	n := 0
	if err := resumePendingOnStart(p, func() error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	update.SaveState(p.dir(), update.State{})
	n = 0
	resumePendingOnStart(p, func() error { n++; return nil })
	if n != 0 {
		t.Fatal("guard launched with nothing pending")
	}
}
