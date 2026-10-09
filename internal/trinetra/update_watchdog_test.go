package trinetra

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// recExec records every command and fails the ones fail says to.
type recExec struct {
	calls [][]string
	fail  func(name string, args []string) error
}

func (r *recExec) Run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.fail != nil {
		return nil, r.fail(name, args)
	}
	return nil, nil
}

func (r *recExec) ran(sub string) bool {
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

func twoBinaryPlan(t *testing.T, p updatePaths) applyPlan {
	t.Helper()
	newCore, newWeb := []byte("NEW-core"), []byte("NEW-web")
	plan := applyPlan{Manifest: update.Manifest{Version: "0.5.0"}, Arch: "amd64",
		Files: []update.File{mf("trinetra-linux-amd64", newCore), mf("trinetra-web-linux-amd64", newWeb)},
		Names: []string{"trinetra", "trinetra-web"}}
	if err := stage(context.Background(), p, mapSource{"trinetra-linux-amd64": newCore, "trinetra-web-linux-amd64": newWeb}, plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return string(b)
}

// TestSwapInRecordsPendingAndPinnedGuardBeforeFirstRename: by the
// time the first binary is replaced, previous/ holds the old build, the
// pinned guard binary is a copy of the binary performing the apply, and
// Pending (phase "swapping", with Files and From) is already on disk -- so a
// crash anywhere after that point is recoverable.
func TestSwapInRecordsPendingAndPinnedGuardBeforeFirstRename(t *testing.T) {
	p := testUpdatePaths(t)
	plan := twoBinaryPlan(t, p)
	var seen bool
	prev := replaceFileFn
	replaceFileFn = func(src, dst string) error {
		if !seen {
			seen = true
			st, _ := update.LoadState(p.dir())
			if st.Pending == nil || st.Pending.Phase != pendingSwapping || st.Pending.Version != "0.5.0" ||
				len(st.Pending.Files) != 2 || st.Pending.From == "" {
				t.Errorf("Pending at first rename = %+v", st.Pending)
			}
			if got := readFile(t, p.guardBin()); got != "SELF-bin" {
				t.Errorf("pinned guard at first rename = %q, want a copy of the applying binary", got)
			}
			if got := readFile(t, filepath.Join(p.previous(), "trinetra")); got != "OLD-core" {
				t.Errorf("previous/trinetra at first rename = %q", got)
			}
		}
		return prev(src, dst)
	}
	t.Cleanup(func() { replaceFileFn = prev })

	if err := swapIn(p, plan, time.Unix(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("no binary was replaced")
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending == nil || st.Pending.Phase != pendingSwapped {
		t.Fatalf("Pending after swap = %+v, want phase %q", st.Pending, pendingSwapped)
	}
	for _, path := range []string{p.guardBin(), p.GuardDir} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("%s: %v %v, want 0755", path, err, fi)
		}
	}
}

// TestSwapInRefusesWithoutInstalledCore: with no regular core binary in
// BinDir there is nothing to snapshot, so nothing to roll back to (I4b).
func TestSwapInRefusesWithoutInstalledCore(t *testing.T) {
	p := testUpdatePaths(t)
	plan := twoBinaryPlan(t, p)
	os.Remove(filepath.Join(p.BinDir, "trinetra"))
	if err := swapIn(p, plan, time.Unix(1000, 0)); err == nil {
		t.Fatal("swapped in with no installed core to keep for rollback")
	}
	if st, _ := update.LoadState(p.dir()); st.Pending != nil {
		t.Fatalf("Pending recorded: %+v", st.Pending)
	}
}

// crashMidSwap runs swapIn and kills it (runtime.Goexit, which like a real
// crash skips the rest of swapIn, including its restore-on-error path) right
// after the first binary is replaced.
func crashMidSwap(t *testing.T, p updatePaths, plan applyPlan) {
	t.Helper()
	prev := replaceFileFn
	n := 0
	replaceFileFn = func(src, dst string) error {
		n++
		if n == 2 {
			runtime.Goexit()
		}
		return prev(src, dst)
	}
	defer func() { replaceFileFn = prev }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		unlock, err := takeApplyLock(p) // released when the "process" dies
		if err != nil {
			t.Error(err)
			return
		}
		defer unlock()
		swapIn(p, plan, time.Unix(1000, 0))
		t.Error("swapIn returned; the simulated crash did not happen")
	}()
	<-done
}

// TestCrashMidSwapThenGuardRollsBack: a crash between renames leaves
// a mixed BinDir with Pending in phase "swapping"; the watchdog's guard
// restores the previous build, restarts, clears Pending and records the
// outcome -- consistently, without marking the version bad (it never ran).
func TestCrashMidSwapThenGuardRollsBack(t *testing.T) {
	p := testUpdatePaths(t)
	plan := twoBinaryPlan(t, p)
	crashMidSwap(t, p, plan)

	if got := readFile(t, filepath.Join(p.BinDir, "trinetra")); got != "NEW-core" {
		t.Fatalf("setup: core = %q, want NEW-core (first rename done)", got)
	}
	if got := readFile(t, filepath.Join(p.BinDir, "trinetra-web")); got != "OLD-web" {
		t.Fatalf("setup: web = %q, want OLD-web (second rename never happened)", got)
	}
	st, _ := update.LoadState(p.dir())
	if st.Pending == nil || st.Pending.Phase != pendingSwapping {
		t.Fatalf("Pending after crash = %+v", st.Pending)
	}

	clk := &fakeClock{t: time.Unix(5000, 0)}
	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: &fakeHealth{active: true, version: "0.5.0", ts: 9999},
		now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err != nil || r.Outcome != "rolled_back" || !strings.Contains(r.Detail, "interrupted") {
		t.Fatalf("guard after crash: r=%+v err=%v", r, err)
	}
	if got := readFile(t, filepath.Join(p.BinDir, "trinetra")); got != "OLD-core" {
		t.Fatalf("core after guard = %q, want OLD-core", got)
	}
	if got := readFile(t, filepath.Join(p.BinDir, "trinetra-web")); got != "OLD-web" {
		t.Fatalf("web after guard = %q", got)
	}
	st, _ = update.LoadState(p.dir())
	if st.Pending != nil || st.IsBad("0.5.0") || st.Floor == "0.5.0" || restarts != 1 {
		t.Fatalf("state after guard = %+v, restarts=%d", st, restarts)
	}
}

// TestGuardLeavesLiveSwapAlone: Pending in phase "swapping" while the apply
// lock is still held means an apply is swapping right now, not that one
// died; the watchdog's guard must leave it alone.
func TestGuardLeavesLiveSwapAlone(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}, Phase: pendingSwapping})
	holdApplyLock(t, p)
	restarts := 0
	_, err := runGuard(guardDeps{paths: p, health: &fakeHealth{}, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if !errors.Is(err, errGuardNotNeeded) || restarts != 0 {
		t.Fatalf("err=%v restarts=%d", err, restarts)
	}
	if st, _ := update.LoadState(p.dir()); st.Pending == nil || readFile(t, filepath.Join(p.BinDir, "trinetra")) != "NEW-core" {
		t.Fatalf("guard touched a live swap: %+v", st)
	}
}

// TestGuardExitsQuietlyWhenAnotherGuardRuns: guard.lock is taken
// before state is read; a second guard (the watchdog firing while the
// launched guard works) exits without touching anything.
func TestGuardExitsQuietlyWhenAnotherGuardRuns(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	unlock, ok, err := update.TryLock(p.guardLock())
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer unlock()
	restarts := 0
	_, err = runGuard(guardDeps{paths: p, health: &fakeHealth{}, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if !errors.Is(err, errGuardNotNeeded) || restarts != 0 {
		t.Fatalf("err=%v restarts=%d", err, restarts)
	}
	if st, _ := update.LoadState(p.dir()); st.Pending == nil {
		t.Fatal("second guard resolved the pending update")
	}
}

// TestGuardResumesPastDeadline: a guard resumed long after
// Pending.Deadline (the watchdog after a reboot, say) still gives the build
// a full health window from its own restart: a healthy build commits, an
// unhealthy one rolls back only after that window.
func TestGuardResumesPastDeadline(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	clk.t = time.Unix(1090+3600, 0)
	r, err := runGuard(guardDeps{paths: p, health: &fakeHealth{active: true, version: "0.5.0", ts: clk.t.Unix() + 1},
		now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("healthy past-deadline resume: r=%+v err=%v", r, err)
	}

	p, clk = guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	start := time.Unix(1090+3600, 0)
	clk.t = start
	r, err = runGuard(guardDeps{paths: p, health: &fakeHealth{active: false},
		now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "rolled_back" {
		t.Fatalf("unhealthy past-deadline resume: r=%+v err=%v", r, err)
	}
	if waited := clk.t.Sub(start); waited < updateHealthDeadline {
		t.Fatalf("rolled back after %v, want a full %v window from the restart", waited, updateHealthDeadline)
	}
}

// slowHealth makes every probe cost probeTimeout of (fake) time, as if each
// one ran into its timeout: Active and Version succeed, the sample is stale.
type slowHealth struct{ clk *fakeClock }

func (s slowHealth) Active() bool { s.clk.sleep(guardProbeTimeout); return true }
func (s slowHealth) Version() (string, error) {
	s.clk.sleep(guardProbeTimeout)
	return "0.5.0", nil
}
func (s slowHealth) SampleTS() (int64, error) { s.clk.sleep(guardProbeTimeout); return 0, nil }

// TestGuardProbesCannotOverrunDeadline: probes are individually
// bounded and none starts after the deadline, so the gate decides no later
// than one probe timeout past its deadline.
func TestGuardProbesCannotOverrunDeadline(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	var decidedAt time.Time
	r, err := runGuard(guardDeps{paths: p, health: slowHealth{clk}, now: clk.now, sleep: clk.sleep,
		restart: func() error { decidedAt = clk.t; return nil }})
	if err != nil || r.Outcome != "rolled_back" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if limit := time.Unix(1090, 0).Add(guardProbeTimeout); decidedAt.After(limit) {
		t.Fatalf("gate decided at %v, more than one probe timeout past the deadline (%v)", decidedAt.Unix(), limit.Unix())
	}
}

// TestRollbackPendingDedupsBadVersions: a
// version that fails its gate twice is listed once, whatever its "v".
func TestRollbackPendingDedupsBadVersions(t *testing.T) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	for _, v := range []string{"0.5.2", "0.5.2", "v0.5.2"} {
		if _, err := rollbackPending(p, update.Pending{Version: v, From: "0.5.1"}, "x", func() error { return nil }, time.Unix(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := update.LoadState(p.dir())
	if len(st.Bad) != 1 || !st.IsBad("0.5.2") || !st.IsBad("v0.5.2") {
		t.Fatalf("bad_versions = %v", st.Bad)
	}
}

// TestGuardLaunchRunsPinnedBinary: the transient guard unit executes
// the pinned guard binary -- never the new build in BinDir, never
// update/previous.
func TestGuardLaunchRunsPinnedBinary(t *testing.T) {
	p := testUpdatePaths(t)
	name, args := guardLaunchCommand(p)
	cmd := name + " " + strings.Join(args, " ")
	if name != "systemd-run" || !strings.Contains(cmd, "--unit trinetra-update-guard") ||
		!strings.Contains(cmd, p.guardBin()+" update guard") {
		t.Fatalf("launch command = %q", cmd)
	}
	if strings.Contains(cmd, filepath.Join(p.BinDir, "trinetra")+" ") || strings.Contains(cmd, p.previous()) {
		t.Fatalf("launch command runs the new or previous build: %q", cmd)
	}
}

// TestWatchdogUnits: a persistent oneshot service running the pinned
// guard with --if-pending, and a timer firing 2 minutes after boot and
// every minute after, enabled and started.
func TestWatchdogUnits(t *testing.T) {
	p := testUpdatePaths(t)
	svc := renderWatchdogService(p.guardBin())
	for _, want := range []string{"Type=oneshot", "ExecStart=" + p.guardBin() + " update guard --if-pending"} {
		if !strings.Contains(svc, want) {
			t.Errorf("service missing %q:\n%s", want, svc)
		}
	}
	tmr := renderWatchdogTimer()
	for _, want := range []string{"OnBootSec=2min", "OnUnitActiveSec=1min", "OnActiveSec=1min", "WantedBy=timers.target"} {
		if !strings.Contains(tmr, want) {
			t.Errorf("timer missing %q:\n%s", want, tmr)
		}
	}
	if strings.Contains(tmr, "Persistent=") {
		t.Errorf("timer should not set Persistent= (only meaningful for OnCalendar= timers; OnActiveSec= makes it fire shortly after every (re)start instead):\n%s", tmr)
	}

	x := &recExec{}
	if err := ensureWatchdog(p, x); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(p.UnitDir, watchdogServiceName)); got != svc {
		t.Fatalf("service file = %q", got)
	}
	if got := readFile(t, filepath.Join(p.UnitDir, watchdogTimerName)); got != tmr {
		t.Fatalf("timer file = %q", got)
	}
	if !x.ran("systemctl daemon-reload") || !x.ran("systemctl enable --now "+watchdogTimerName) {
		t.Fatalf("systemctl calls = %v", x.calls)
	}
	// Unchanged units: no reload needed, the timer is still (re)enabled.
	x2 := &recExec{}
	if err := ensureWatchdog(p, x2); err != nil {
		t.Fatal(err)
	}
	if x2.ran("daemon-reload") || !x2.ran("enable --now "+watchdogTimerName) {
		t.Fatalf("second ensure calls = %v", x2.calls)
	}
}

// TestApplyEnsuresWatchdogFirst: apply makes sure the watchdog is installed
// before it swaps anything, and aborts untouched if it cannot.
func TestApplyEnsuresWatchdogFirst(t *testing.T) {
	p := testUpdatePaths(t)
	src := signedRelease(t, "0.5.0", map[string][]byte{"trinetra-linux-amd64": []byte("NEW-core"), "trinetra-web-linux-amd64": []byte("NEW-web")})
	sys := &recExec{fail: func(string, []string) error { return errors.New("systemctl: boom") }}
	u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), sys: sys, now: func() time.Time { return time.Unix(1000, 0) },
		arch: "amd64", running: mustVer("0.4.1"), launchGuard: func() error { return nil }, src: src}
	if _, err := u.apply(context.Background(), config.Default(), applyOptions{Version: "0.5.0"}); err == nil || !strings.Contains(err.Error(), "watchdog") {
		t.Fatalf("apply with a failing watchdog install: %v", err)
	}
	if got := readFile(t, filepath.Join(p.BinDir, "trinetra")); got != "OLD-core" {
		t.Fatalf("binary replaced despite no watchdog: %q", got)
	}
	if st, _ := update.LoadState(p.dir()); st.Pending != nil {
		t.Fatalf("Pending recorded: %+v", st.Pending)
	}
}

// TestApplyGuardLaunchFailure is R16/M19: when the guard cannot be
// launched, apply restores the previous build and clears Pending -- unless
// a guard is already running (the watchdog got there first), which then
// owns the pending update.
func TestApplyGuardLaunchFailure(t *testing.T) {
	for _, guardRunning := range []bool{false, true} {
		p := testUpdatePaths(t)
		src := signedRelease(t, "0.5.0", map[string][]byte{"trinetra-linux-amd64": []byte("NEW-core"), "trinetra-web-linux-amd64": []byte("NEW-web")})
		launch := func() error { return errors.New("systemd-run: boom") }
		if guardRunning {
			launch = func() error {
				unlock, _, _ := update.TryLock(p.guardLock())
				t.Cleanup(unlock)
				return errors.New("systemd-run: boom")
			}
		}
		u := updater{paths: p, keys: testKeys(), x: fakeVersionExec("0.5.0"), now: func() time.Time { return time.Unix(1000, 0) },
			arch: "amd64", running: mustVer("0.4.1"), launchGuard: launch, src: src}
		_, err := u.apply(context.Background(), config.Default(), applyOptions{Version: "0.5.0"})
		if err == nil {
			t.Fatalf("guardRunning=%v: apply succeeded without a guard", guardRunning)
		}
		st, _ := update.LoadState(p.dir())
		core := readFile(t, filepath.Join(p.BinDir, "trinetra"))
		if guardRunning {
			if st.Pending == nil || core != "NEW-core" {
				t.Fatalf("running guard's update was undone: pending=%+v core=%q", st.Pending, core)
			}
		} else if st.Pending != nil || core != "OLD-core" {
			t.Fatalf("not rolled back: pending=%+v core=%q", st.Pending, core)
		}
	}
}

// TestRollbackFailurePaths: if restoring previous/ fails the
// binaries may be half-restored, so Pending stays (phase "swapping") for the
// watchdog to finish; if the restore worked but the guard cannot start,
// Pending is cleared and the operator is told to restart.
func TestRollbackFailurePaths(t *testing.T) {
	setup := func(t *testing.T) (updatePaths, updater) {
		p := testUpdatePaths(t)
		os.MkdirAll(p.previous(), 0o700)
		os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("PREV-core"), 0o755)
		return p, updater{paths: p, x: fakeVersionExec("0.4.1"), now: func() time.Time { return time.Unix(1000, 0) },
			running: mustVer("0.5.0"), launchGuard: func() error { return errors.New("systemd-run: boom") }}
	}

	t.Run("restore fails", func(t *testing.T) {
		p, u := setup(t)
		chmodUnwritable(t, p.BinDir)
		err := u.rollback()
		if err == nil || !strings.Contains(err.Error(), "watchdog") {
			t.Fatalf("rollback = %v", err)
		}
		st, _ := update.LoadState(p.dir())
		if st.Pending == nil || !st.Pending.Rollback || st.Pending.Phase != pendingSwapping {
			t.Fatalf("Pending = %+v, want kept in phase swapping", st.Pending)
		}
	})
	t.Run("guard fails to start", func(t *testing.T) {
		p, u := setup(t)
		err := u.rollback()
		if err == nil || !strings.Contains(err.Error(), "restart") {
			t.Fatalf("rollback = %v", err)
		}
		st, _ := update.LoadState(p.dir())
		if st.Pending != nil || readFile(t, filepath.Join(p.BinDir, "trinetra")) != "PREV-core" {
			t.Fatalf("Pending=%+v core=%q", st.Pending, readFile(t, filepath.Join(p.BinDir, "trinetra")))
		}
	})
}

// TestInterruptedRollbackIsFinishedByGuard: a rollback whose restore was
// interrupted (phase "swapping", Rollback) is finished by the guard, which
// then gates the restored build as usual.
func TestInterruptedRollbackIsFinishedByGuard(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.4.1", From: "0.5.0", Deadline: 1090, Files: []string{"trinetra"}, Rollback: true, Phase: pendingSwapping})
	r, err := runGuard(guardDeps{paths: p, health: &fakeHealth{active: true, version: "0.4.1", ts: 1002},
		now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
	if err != nil || r.Outcome != "committed" {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if got := readFile(t, filepath.Join(p.BinDir, "trinetra")); got != "OLD-core" {
		t.Fatalf("core = %q, want the restored previous build", got)
	}
}
