// Package trinetra: update_guard.go is `trinetra update guard`'s state
// machine. It always runs from the pinned guard binary
// (/usr/local/lib/trinetra/guard/trinetra, a copy of the binary that wrote
// the Pending), never the new build: launched via systemd-run right after
// apply/rollback swap a build in, and every minute by the persistent
// trinetra-update-watchdog.timer (`update guard --if-pending`), which is what
// resolves a pending update after a killed guard, a crash mid-swap or a
// reboot (R14). It takes update/guard.lock before reading state, so only one
// guard ever works on a Pending. It restarts the daemon onto the pending
// build, polls the running daemon's health until the deadline, then commits
// (raises the floor, clears Pending) or rolls back (restores the previous
// build, marks the version bad, clears Pending).
package trinetra

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// guardHealthPollInterval is how often runGuard polls guardHealth while
// waiting out a pending update's health-gate deadline.
const guardHealthPollInterval = 2 * time.Second

// guardProbeTimeout bounds each health probe (systemctl is-active, each
// control-socket call), so the gate never overruns its deadline by more than
// one probe timeout (R14).
const guardProbeTimeout = 5 * time.Second

// errGuardNotNeeded is runGuard's quiet "nothing for me to do right now":
// another guard holds guard.lock, or an apply/rollback is still swapping in
// a live process (it holds apply.lock). The watchdog timer fires every
// minute, so the next run picks up anything still pending.
var errGuardNotNeeded = errors.New("update guard: another guard or update is already working on the pending update")

// guardHealth is what runGuard polls to decide whether the freshly-restarted
// daemon is healthy: the systemd unit is active, it reports the version this
// Pending update expects, and its sampler loop has produced a fresh sample
// since the restart. The production implementation (systemdControlHealth,
// below) shells out to systemctl for Active and dials the control socket
// fresh for Version/SampleTS -- the guard runs as its own systemd-run unit,
// not inside the daemon process, so it cannot share a live control.Client.
type guardHealth interface {
	Active() bool
	Version() (string, error)
	SampleTS() (int64, error)
}

// guardDeps bundles everything runGuard needs, all of it injectable for
// tests. x is carried alongside health for symmetry with the rest of this
// package's dependency bundles (updater.x, e.g.) and is available to a
// caller building guardDeps in production; runGuard's own state machine
// only ever calls health/now/sleep/restart.
type guardDeps struct {
	paths   updatePaths
	x       Exec
	health  guardHealth
	now     func() time.Time
	sleep   func(time.Duration)
	restart func() error
}

// runGuard is the whole guarded-restart state machine:
//
//  1. Take guard.lock (non-blocking; held -> errGuardNotNeeded), then load
//     state; no Pending means there is nothing to guard -- return a zero
//     Result.
//  2. Pending in phase "swapping": if apply.lock is held an apply/rollback
//     is still swapping (errGuardNotNeeded). Otherwise it died mid-swap: a
//     forward update is rolled back at once (resolveInterruptedSwap); an
//     interrupted rollback has its restore finished and is then gated below.
//  3. Restart the daemon onto the pending build.
//  4. Poll every guardHealthPollInterval until the deadline
//     (max(Pending.Deadline, restart-time+90s), so a guard resumed after a
//     crash or reboot always gets the full window from ITS restart):
//     healthy iff the unit is active, reports the pending version, and has
//     produced a sample since the restart. Each probe is bounded and none
//     starts after the deadline.
//  5. Healthy -> commit: raise the floor (unless this Pending is itself a
//     rollback), clear Pending, remove the staged download, record Last.
//  6. Deadline passed -> roll back: restore the previous build and restart
//     onto it (unless this Pending is itself a rollback -- see
//     rollbackPending), mark the version bad (again, unless a rollback),
//     clear Pending, record Last with the first failing health condition.
func runGuard(d guardDeps) (update.Result, error) {
	unlock, ok, err := update.TryLock(d.paths.guardLock())
	if err != nil {
		return update.Result{}, err
	}
	if !ok {
		return update.Result{}, errGuardNotNeeded
	}
	defer unlock()

	st, err := update.LoadState(d.paths.dir())
	if err != nil {
		return update.Result{}, err
	}
	if st.Pending == nil {
		return update.Result{}, nil
	}
	pending := *st.Pending

	if pending.Phase == pendingSwapping {
		unlockApply, ok, err := update.TryLock(d.paths.applyLock())
		if err != nil {
			return update.Result{}, err
		}
		if !ok {
			return update.Result{}, errGuardNotNeeded
		}
		if !pending.Rollback {
			defer unlockApply()
			return resolveInterruptedSwap(d, pending)
		}
		// An interrupted `trinetra update rollback`: finish restoring
		// previous/, then confirm it through the normal gate.
		err = restorePrevious(d.paths)
		if err == nil {
			pending.Phase = pendingSwapped
			err = setPending(d.paths, &pending)
		}
		unlockApply()
		if err != nil {
			return update.Result{}, fmt.Errorf("update guard: finishing an interrupted rollback failed (the watchdog will retry): %w", err)
		}
	}

	restartedAt := d.now()
	// A restart() error is recorded as a failure detail but is NOT fatal:
	// osExec's own timeout (60s, execTimeout) is shorter than systemd's
	// default TimeoutStopSec (90s), so `systemctl restart trinetra` can
	// return an error purely because the command itself timed out waiting,
	// while systemd goes on to finish the restart successfully a few
	// seconds later. Keep polling: a genuinely healthy restart still
	// commits, and a genuinely broken one still rolls back at the deadline.
	var lastDetail string
	if err := d.restart(); err != nil {
		lastDetail = "restart: " + err.Error()
	}

	deadline := pending.Deadline
	if min := restartedAt.Add(healthDeadline()).Unix(); min > deadline {
		deadline = min
	}
	expired := func() bool { return d.now().Unix() > deadline }

	for {
		if ok, detail := checkGuardHealth(d.health, pending.Version, restartedAt, expired); ok {
			return commitPending(d.paths, pending, d.now())
		} else if detail != "" {
			lastDetail = detail
		}
		if expired() {
			break
		}
		d.sleep(guardHealthPollInterval)
	}
	return rollbackPending(d.paths, pending, lastDetail, d.restart, d.now())
}

// resolveInterruptedSwap handles a forward update whose apply died mid-swap
// (R15): BinDir may hold a mix of old and new binaries and the new build
// never passed its gate, so restore previous/, restart onto it, clear
// Pending and record the outcome. The version is not marked bad -- it was
// never tried. If the restore fails, Pending is kept for the next run.
func resolveInterruptedSwap(d guardDeps, pending update.Pending) (update.Result, error) {
	if err := restorePrevious(d.paths); err != nil {
		return update.Result{}, fmt.Errorf("update guard: restoring the previous build after an interrupted swap failed (the watchdog will retry): %w", err)
	}
	detail := "the update was interrupted during the swap (the apply did not finish); the previous build was restored"
	if err := d.restart(); err != nil {
		detail += "; restart after restoring failed: " + err.Error()
	}
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "rolled_back", Detail: detail, At: d.now().Unix()}
	err := update.WithState(d.paths.dir(), func(st *update.State) error {
		st.Last = &result
		st.Pending = nil
		return nil
	})
	if err != nil {
		return update.Result{}, err
	}
	auditUpdate(d.paths, guardActor, "update.rolled_back", pending.Version, guardAuditDetail(pending, detail), d.now())
	return result, nil
}

// guardAuditDetail is the detail of the guard's audit entries.
func guardAuditDetail(pending update.Pending, detail string) string {
	out := "from " + pending.From
	if pending.Rollback {
		out += " (manual rollback)"
	}
	if detail != "" {
		out += ": " + detail
	}
	return out
}

// checkGuardHealth reports whether h looks healthy for a Pending targeting
// wantVersion restarted at restartedAt, and -- when it isn't -- the first
// failing condition, for Result.Detail. Once expired() reports the deadline
// has passed, no further probe is started (detail is then "" and the
// caller keeps the last real failure).
func checkGuardHealth(h guardHealth, wantVersion string, restartedAt time.Time, expired func() bool) (ok bool, detail string) {
	if !h.Active() {
		return false, "trinetra.service is not active"
	}
	if expired() {
		return false, ""
	}
	v, err := h.Version()
	if err != nil {
		return false, "could not reach the control socket: " + err.Error()
	}
	// Normalise "v" on BOTH sides before comparing, the same way smokeTest
	// does (update_apply.go).
	want := strings.TrimPrefix(wantVersion, "v")
	if got := strings.TrimPrefix(v, "v"); got != want {
		return false, fmt.Sprintf("reported %s, want %s", got, want)
	}
	if expired() {
		return false, ""
	}
	ts, err := h.SampleTS()
	if err != nil {
		return false, "could not read a snapshot: " + err.Error()
	}
	if ts <= restartedAt.Unix() {
		return false, "no fresh sample since the restart"
	}
	return true, ""
}

// commitPending marks a healthy Pending committed: raises the floor (unless
// it is itself a rollback confirmation -- rollback semantics never raise the
// floor), clears Pending, removes the now-consumed staged download, and
// records Last.
func commitPending(p updatePaths, pending update.Pending, now time.Time) (update.Result, error) {
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "committed", At: now.Unix()}
	err := update.WithState(p.dir(), func(st *update.State) error {
		if !pending.Rollback {
			if v, verr := update.ParseVersion(pending.Version); verr == nil {
				st.RaiseFloor(v)
			}
		}
		st.Last = &result
		st.Pending = nil
		return nil
	})
	if err != nil {
		return update.Result{}, err
	}
	auditUpdate(p, guardActor, "update.commit", pending.Version, guardAuditDetail(pending, ""), now)
	_ = os.RemoveAll(p.staging(pending.Version))
	return result, nil
}

// rollbackPending handles a Pending that failed its health gate.
//
// For a forward update (pending.Rollback == false): restore the previous
// build, restart onto it, mark the version bad, and never raise the floor.
// If restoring the previous build itself fails (#136), Pending is kept --
// not cleared like every other outcome here -- with the failure recorded in
// Pending.RestoreFailed, so the next watchdog tick re-enters this same state
// machine and retries the restore; nothing is marked bad and no Last/audit
// entry is written, because this update has not actually finished rolling
// back yet. The critical alert for that failure is raised once by the
// daemon's update loop (notifyRestoreFailed), deduped via
// Pending.RestoreFailedNotified -- the guard itself is a separate,
// short-lived process launched fresh for each retry and cannot notify
// directly or remember across runs. A later retry whose restore succeeds
// falls through to the normal rollback below (Pending cleared entirely,
// version marked bad, outcome rolled_back).
//
// For a rollback confirmation (pending.Rollback == true): there is no older
// build to fall back to -- previous/ holds exactly the build that is
// already running (rollback() restored it before launching the guard) -- so
// this leaves the binaries as they are and does not loop: no further
// restore, no bad-version mark (rollback semantics never mark a version
// bad), just record the failure.
func rollbackPending(p updatePaths, pending update.Pending, detail string, restart func() error, now time.Time) (update.Result, error) {
	if !pending.Rollback {
		restoreErr := restorePrevious(p)
		if restoreErr != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "restoring the previous build also failed: " + restoreErr.Error()
		}
		if err := restart(); err != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "restart after rollback also failed: " + err.Error()
		}
		if restoreErr != nil {
			pending.RestoreFailed = detail
			if err := setPending(p, &pending); err != nil {
				return update.Result{}, err
			}
			return update.Result{}, fmt.Errorf("update guard: restoring the previous build failed (the watchdog will retry): %w", restoreErr)
		}
	}

	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "rolled_back", Detail: detail, At: now.Unix()}
	err := update.WithState(p.dir(), func(st *update.State) error {
		if !pending.Rollback {
			st.MarkBad(pending.Version)
		}
		st.Last = &result
		st.Pending = nil
		return nil
	})
	if err != nil {
		return update.Result{}, err
	}
	auditUpdate(p, guardActor, "update.rolled_back", pending.Version, guardAuditDetail(pending, detail), now)
	return result, nil
}

// systemdControlHealth is the production guardHealth: Active via `systemctl
// is-active --quiet trinetra` (through x, a timeoutExec bounded by
// guardProbeTimeout), Version/SampleTS via a fresh control-socket dial each
// call, each bounded by guardProbeTimeout (R14) -- the guard runs standalone,
// not inside the daemon process, so it cannot share a live control.Client.
type systemdControlHealth struct{ x Exec }

func (h systemdControlHealth) Active() bool {
	_, err := h.x.Run("systemctl", "is-active", "--quiet", "trinetra")
	return err == nil
}

// boundedProbe runs fn with guardProbeTimeout. On timeout the call is
// abandoned (its goroutine finishes, or hits the control client's own
// deadline, in the background of this short-lived guard process).
func boundedProbe[T any](fn func() (T, error)) (T, error) {
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		v, err := fn()
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-time.After(guardProbeTimeout):
		var zero T
		return zero, fmt.Errorf("no answer within %s", guardProbeTimeout)
	}
}

func (h systemdControlHealth) dial() (*control.Client, error) {
	tok, _ := os.ReadFile(controlTokenPath())
	return control.Dial(controlSocketPath(), strings.TrimSpace(string(tok)))
}

func (h systemdControlHealth) Version() (string, error) {
	return boundedProbe(func() (string, error) {
		c, err := h.dial()
		if err != nil {
			return "", err
		}
		defer c.Close()
		return c.Version()
	})
}

func (h systemdControlHealth) SampleTS() (int64, error) {
	return boundedProbe(func() (int64, error) {
		c, err := h.dial()
		if err != nil {
			return 0, err
		}
		defer c.Close()
		v, err := c.Snapshot()
		if err != nil {
			return 0, err
		}
		return v.TS, nil
	})
}

// realGuardDeps builds the production guardDeps: real paths, the real
// (timeout-bounded) Exec, systemdControlHealth, the real clock/sleep, and a
// restart that shells out to `systemctl restart trinetra`.
func realGuardDeps() guardDeps {
	x := osExec{}
	return guardDeps{
		paths:  defaultUpdatePaths(),
		x:      x,
		health: systemdControlHealth{x: timeoutExec{guardProbeTimeout}},
		now:    time.Now,
		sleep:  time.Sleep,
		restart: func() error {
			// TRINETRA_E2E_RESTART_CMD (trinetra_testkeys builds only -- see
			// update_e2e_hooks.go/update_e2e_hooks_testkeys.go) replaces
			// `systemctl restart trinetra` for the docker e2e harness's
			// systemd-less host.
			if cmd, args, ok := e2eRestartCmd(); ok {
				_, err := x.Run(cmd, args...)
				return err
			}
			_, err := x.Run("systemctl", "restart", "trinetra")
			return err
		},
	}
}

// cmdUpdateGuard is `trinetra update guard [--if-pending]`, run from the
// pinned guard binary: by the transient unit launchGuard starts right after
// apply/rollback swap a build in, and (with --if-pending) every minute by
// trinetra-update-watchdog.service. It runs the health-gate state machine to
// completion (confirm or roll back) and prints the outcome. --if-pending is
// silent (exit 0) when nothing is pending or another guard/update is
// already on it, so the timer's runs stay quiet and never "fail".
func cmdUpdateGuard(args []string) int {
	ifPending := false
	for _, a := range args {
		switch a {
		case "--if-pending":
			ifPending = true
		default:
			fmt.Fprintf(stderr, "unknown update guard flag %q\nusage: update guard [--if-pending]\n", a)
			return 2
		}
	}
	if !isRoot() {
		fmt.Fprintln(stderr, "must run as root")
		return 1
	}
	r, err := runGuard(realGuardDeps())
	if errors.Is(err, errGuardNotNeeded) {
		if !ifPending {
			fmt.Fprintln(stdout, err)
		}
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "update guard:", err)
		return 1
	}
	if r.Outcome == "" {
		if !ifPending {
			fmt.Fprintln(stdout, "update guard: nothing pending")
		}
		return 0
	}
	if r.Detail != "" {
		fmt.Fprintf(stdout, "update guard: %s %s (%s)\n", r.Version, r.Outcome, r.Detail)
	} else {
		fmt.Fprintf(stdout, "update guard: %s %s\n", r.Version, r.Outcome)
	}
	if r.Outcome != "committed" {
		return 1
	}
	return 0
}
