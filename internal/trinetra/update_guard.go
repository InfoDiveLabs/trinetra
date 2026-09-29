// Package trinetra: update_guard.go is `trinetra update guard`'s state
// machine -- launched via systemd-run right after apply/rollback swap a
// build in (see updater.apply/rollback's launchGuard), and again at daemon
// start (resumePendingOnStart) to resume across a crash mid-guard. It
// restarts the daemon onto the pending build, polls the running daemon's own
// health over the control socket for up to the pending update's deadline,
// then either commits (raises the floor, clears Pending) or rolls back
// (restores the previous build, marks the version bad, clears Pending). It
// never leaves Pending set when it returns.
package trinetra

import (
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

// runGuard is the whole guarded-restart state machine (task 7 brief):
//
//  1. Load state; no Pending means there is nothing to guard -- return a
//     zero Result.
//  2. Restart the daemon onto the pending build.
//  3. Poll every guardHealthPollInterval until the deadline
//     (max(Pending.Deadline, restart-time+90s), so a guard resumed after a
//     crash always gets the full 90s window from ITS restart, even if the
//     original deadline has already nearly or fully elapsed by wall clock):
//     healthy iff the unit is active, reports the pending version, and has
//     produced a sample since the restart.
//  4. Healthy -> commit: raise the floor (unless this Pending is itself a
//     rollback), clear Pending, remove the staged download, record Last.
//  5. Deadline passed -> roll back: restore the previous build and restart
//     onto it (unless this Pending is itself a rollback -- see
//     rollbackPending), mark the version bad (again, unless a rollback),
//     clear Pending, record Last with the first failing health condition.
//
// Every state write re-reads state first (LoadState -> modify -> SaveState)
// so a concurrent CLI/daemon write is never clobbered.
func runGuard(d guardDeps) (update.Result, error) {
	st, err := update.LoadState(d.paths.dir())
	if err != nil {
		return update.Result{}, err
	}
	if st.Pending == nil {
		return update.Result{}, nil
	}
	pending := *st.Pending

	restartedAt := d.now()
	// A restart() error is recorded as a failure detail but is NOT fatal:
	// osExec's own timeout (60s, execTimeout) is shorter than systemd's
	// default TimeoutStopSec (90s), so `systemctl restart trinetra` can
	// return an error purely because the command itself timed out waiting,
	// while systemd goes on to finish the restart successfully a few
	// seconds later. Bailing out here would leave an unverified binary
	// installed with Pending set and no guard watching it -- instead, keep
	// polling: a genuinely healthy restart still commits, and a genuinely
	// broken one still rolls back once the deadline passes, exactly as if
	// restart() had returned nil and the daemon just never came up.
	var lastDetail string
	if err := d.restart(); err != nil {
		lastDetail = "restart: " + err.Error()
	}

	deadline := pending.Deadline
	if min := restartedAt.Add(updateHealthDeadline).Unix(); min > deadline {
		deadline = min
	}

	for {
		if ok, detail := checkGuardHealth(d.health, pending.Version, restartedAt); ok {
			return commitPending(d.paths, pending, d.now())
		} else {
			lastDetail = detail
		}
		if d.now().Unix() > deadline {
			break
		}
		d.sleep(guardHealthPollInterval)
	}
	return rollbackPending(d.paths, pending, lastDetail, d.restart, d.now())
}

// checkGuardHealth reports whether h looks healthy for a Pending targeting
// wantVersion restarted at restartedAt, and -- when it isn't -- the first
// failing condition, for Result.Detail.
func checkGuardHealth(h guardHealth, wantVersion string, restartedAt time.Time) (ok bool, detail string) {
	if !h.Active() {
		return false, "trinetra.service is not active"
	}
	v, err := h.Version()
	if err != nil {
		return false, "could not reach the control socket: " + err.Error()
	}
	// Normalise "v" on BOTH sides before comparing, the same way smokeTest
	// does (update_apply.go): wantVersion is Pending.Version, whose producers
	// are not all guaranteed to have stripped a leading "v" (fix-round-1 F1 --
	// updater.rollback() used to store `version --json`'s raw, v-prefixed
	// output verbatim, which made every rollback's health gate misreport a
	// version mismatch and roll back a perfectly healthy restart).
	want := strings.TrimPrefix(wantVersion, "v")
	if got := strings.TrimPrefix(v, "v"); got != want {
		return false, fmt.Sprintf("reported %s, want %s", got, want)
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
	st, err := update.LoadState(p.dir())
	if err != nil {
		return update.Result{}, err
	}
	if !pending.Rollback {
		if v, verr := update.ParseVersion(pending.Version); verr == nil {
			st.RaiseFloor(v)
		}
	}
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "committed", At: now.Unix()}
	st.Last = &result
	st.Pending = nil
	if err := update.SaveState(p.dir(), st); err != nil {
		return update.Result{}, err
	}
	_ = os.RemoveAll(p.staging(pending.Version))
	return result, nil
}

// rollbackPending handles a Pending that failed its health gate.
//
// For a forward update (pending.Rollback == false): restore the previous
// build, restart onto it, mark the version bad, and never raise the floor.
//
// For a rollback confirmation (pending.Rollback == true): there is no older
// build to fall back to -- previous/ holds exactly the build that is
// already running (rollback() restored it before launching the guard) -- so
// this leaves the binaries as they are and does not loop: no further
// restore, no bad-version mark (rollback semantics never mark a version
// bad), just record the failure.
func rollbackPending(p updatePaths, pending update.Pending, detail string, restart func() error, now time.Time) (update.Result, error) {
	if !pending.Rollback {
		if err := restorePrevious(p); err != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "restoring the previous build also failed: " + err.Error()
		}
		if err := restart(); err != nil {
			if detail != "" {
				detail += "; "
			}
			detail += "restart after rollback also failed: " + err.Error()
		}
	}

	st, err := update.LoadState(p.dir())
	if err != nil {
		return update.Result{}, err
	}
	if !pending.Rollback {
		st.Bad = append(st.Bad, pending.Version)
	}
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "rolled_back", Detail: detail, At: now.Unix()}
	st.Last = &result
	st.Pending = nil
	if err := update.SaveState(p.dir(), st); err != nil {
		return update.Result{}, err
	}
	return result, nil
}

// resumePendingOnStart is the daemon-start hook (daemon.go): if state has a
// Pending update recorded (the daemon crashed, or was killed, mid-guard, or
// mid-apply before the guard even started), launch a fresh guard for it. A
// missing/unreadable state, or no Pending, is a no-op.
func resumePendingOnStart(p updatePaths, launch func() error) error {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return err
	}
	if st.Pending == nil {
		return nil
	}
	return launch()
}

// systemdControlHealth is the production guardHealth: Active via `systemctl
// is-active --quiet trinetra` (through x, so it shares the same timeout
// discipline as every other shelled-out command), Version/SampleTS via a
// fresh control-socket dial each call -- the guard runs standalone under
// systemd-run, not inside the daemon process, so it cannot share a live
// control.Client the way an in-process caller would.
type systemdControlHealth struct{ x Exec }

func (h systemdControlHealth) Active() bool {
	_, err := h.x.Run("systemctl", "is-active", "--quiet", "trinetra")
	return err == nil
}

func (h systemdControlHealth) dial() (*control.Client, error) {
	tok, _ := os.ReadFile(controlTokenPath())
	return control.Dial(controlSocketPath(), strings.TrimSpace(string(tok)))
}

func (h systemdControlHealth) Version() (string, error) {
	c, err := h.dial()
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.Version()
}

func (h systemdControlHealth) SampleTS() (int64, error) {
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
}

// realGuardDeps builds the production guardDeps: real paths, the real
// (timeout-bounded) Exec, systemdControlHealth, the real clock/sleep, and a
// restart that shells out to `systemctl restart trinetra`.
func realGuardDeps() guardDeps {
	x := osExec{}
	return guardDeps{
		paths:  defaultUpdatePaths(),
		x:      x,
		health: systemdControlHealth{x: x},
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

// cmdUpdateGuard is `trinetra update guard`: what launchGuard starts via
// systemd-run right after apply/rollback swap a build in. It runs the
// health-gate state machine to completion (confirm or roll back) and prints
// the outcome.
func cmdUpdateGuard(args []string) int {
	if !isRoot() {
		fmt.Fprintln(stderr, "must run as root")
		return 1
	}
	r, err := runGuard(realGuardDeps())
	if err != nil {
		fmt.Fprintln(stderr, "update guard:", err)
		return 1
	}
	if r.Outcome == "" {
		fmt.Fprintln(stdout, "update guard: nothing pending")
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
