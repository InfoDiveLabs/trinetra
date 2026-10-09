// Package trinetra: update_guard.go is `trinetra update guard`'s state
// machine. It always runs from the pinned guard binary
// (/usr/local/lib/trinetra/guard/trinetra, a copy of the binary that wrote
// the Pending), never the new build: launched via systemd-run right after
// apply/rollback swap a build in, and every minute by the persistent
// trinetra-update-watchdog.timer (`update guard --if-pending`), which is what
// resolves a pending update after a killed guard, a crash mid-swap or a
// reboot. It takes update/guard.lock before reading state, so only one
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

// guardProbeTimeout bounds each health probe (systemctl is-active, each control-socket
// call), so the gate never overruns its deadline by more than one probe timeout.
const guardProbeTimeout = 5 * time.Second

// errGuardNotNeeded is runGuard's quiet "nothing for me to do right now": another guard
// holds guard.lock, or an apply/rollback is still swapping in a live process.
var errGuardNotNeeded = errors.New("update guard: another guard or update is already working on the pending update")

// guardHealth is what runGuard polls to decide whether the freshly-restarted daemon is
// healthy: the systemd unit is active, it reports the version this Pending update expects.
type guardHealth interface {
	Active() bool
	Version() (string, error)
	SampleTS() (int64, error)
}

// guardDeps bundles everything runGuard needs, all of it injectable for tests. x is carried
// alongside health for symmetry with the rest of this package's dependency bundles.
type guardDeps struct {
	paths   updatePaths
	x       Exec
	health  guardHealth
	now     func() time.Time
	sleep   func(time.Duration)
	restart func() error
}

// runGuard is the whole guarded-restart state machine:
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

	// #136: once this Pending has already failed its health gate AND a restore attempt, the
	// rollback decision is final.
	if !pending.Rollback && pending.RestoreFailed != "" {
		return retryFailedRestore(d, pending)
	}

	restartedAt := d.now()
	// A restart() error is recorded as a failure detail but is NOT fatal: osExec's own timeout
	// (60s, execTimeout) is shorter than systemd's default TimeoutStopSec (90s).
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

// resolveInterruptedSwap handles a forward update whose apply died mid-swap : BinDir may
// hold a mix of old and new binaries and the new build never passed its gate.
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
		st.RecordResult(result)
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

// checkGuardHealth reports whether h looks healthy for a Pending targeting wantVersion
// restarted at restartedAt, and -- when it isn't -- the first failing condition.
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

// commitPending marks a healthy Pending committed: raises the floor (unless it is itself a
// rollback confirmation -- rollback semantics never raise the floor), clears Pending.
func commitPending(p updatePaths, pending update.Pending, now time.Time) (update.Result, error) {
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "committed", At: now.Unix()}
	err := update.WithState(p.dir(), func(st *update.State) error {
		if !pending.Rollback {
			if v, verr := update.ParseVersion(pending.Version); verr == nil {
				st.RaiseFloor(v)
			}
		}
		st.RecordResult(result)
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

// rollbackPending handles a Pending that just failed its health gate (the restart-and-poll
// loop above has just run).
func rollbackPending(p updatePaths, pending update.Pending, detail string, restart func() error, now time.Time) (update.Result, error) {
	if !pending.Rollback {
		reason := detail
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
			pending.RestoreFailedReason = reason
			if err := setPending(p, &pending); err != nil {
				return update.Result{}, err
			}
			return update.Result{}, fmt.Errorf("update guard: restoring the previous build failed (the watchdog will retry): %w", restoreErr)
		}
	}
	return finishRollback(p, pending, detail, now)
}

// retryFailedRestore is runGuard's path for a Pending whose restore has already failed once
// (#136): the guard's decision to roll back is final, so this retries ONLY restorePrevious.
func retryFailedRestore(d guardDeps, pending update.Pending) (update.Result, error) {
	reason := pending.RestoreFailedReason
	if err := restorePrevious(d.paths); err != nil {
		detail := reason
		if detail != "" {
			detail += "; "
		}
		detail += "restoring the previous build also failed: " + err.Error()
		pending.RestoreFailed = detail
		if err := setPending(d.paths, &pending); err != nil {
			return update.Result{}, err
		}
		return update.Result{}, fmt.Errorf("update guard: retrying the previous-build restore failed (the watchdog will retry): %w", err)
	}
	detail := reason
	if err := d.restart(); err != nil {
		if detail != "" {
			detail += "; "
		}
		detail += "restart after rollback also failed: " + err.Error()
	}
	return finishRollback(d.paths, pending, detail, d.now())
}

// finishRollback records a Pending as rolled back once its restore has
// actually succeeded (whether on the first attempt, inside rollbackPending,
// or on a later watchdog retry, inside retryFailedRestore): marks the
// version bad (unless it is a rollback confirmation -- rollback semantics
// never mark a version bad), clears Pending entirely (RestoreFailed and
// RestoreFailedReason go with it), records Last, and audits.
func finishRollback(p updatePaths, pending update.Pending, detail string, now time.Time) (update.Result, error) {
	result := update.Result{Version: pending.Version, From: pending.From, Outcome: "rolled_back", Detail: detail, At: now.Unix()}
	err := update.WithState(p.dir(), func(st *update.State) error {
		if !pending.Rollback {
			st.MarkBad(pending.Version)
		}
		st.RecordResult(result)
		st.Pending = nil
		return nil
	})
	if err != nil {
		return update.Result{}, err
	}
	auditUpdate(p, guardActor, "update.rolled_back", pending.Version, guardAuditDetail(pending, detail), now)
	return result, nil
}

// systemdControlHealth is the production guardHealth: Active via `systemctl is-active
// --quiet trinetra` (through x, a timeoutExec bounded by guardProbeTimeout).
type systemdControlHealth struct{ x Exec }

func (h systemdControlHealth) Active() bool {
	_, err := h.x.Run("systemctl", "is-active", "--quiet", "trinetra")
	return err == nil
}

// boundedProbe runs fn with guardProbeTimeout.
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

// realGuardDeps builds the production guardDeps: real paths, the real (timeout-bounded)
// Exec, systemdControlHealth, the real clock/sleep.
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

// cmdUpdateGuard is `trinetra update guard [--if-pending]`, run from the pinned guard
// binary.
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
