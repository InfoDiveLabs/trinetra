// Package trinetra: update_daemon.go wires self-update into the running
// daemon: resuming a pending update at start (via resumePendingOnStart,
// update_guard.go) and the periodic loop that checks for a new release and
// turns update.State transitions into operator-facing Alerts ("update
// available", "updated"/"rolled back", and a stale-pointer warning).
package trinetra

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// updateLoopInterval is how often startUpdateLoop wakes to notify a
// not-yet-notified result, run a due update.channel check, and notify a
// newly available version.
const updateLoopInterval = 5 * time.Minute

// stalePointerAfter is how long with no fresh channel pointer (State's
// LastPointerIssued) is considered stale enough to warn about, even though
// ordinary check() errors are otherwise only logged.
const stalePointerAfter = 14 * 24 * time.Hour

// updateResultAlert maps a not-yet-notified update.Result to the Alert
// startUpdateLoop delivers, and reports false for a Result that carries no
// alert (already notified, or an outcome this build doesn't know about --
// defensive against a future outcome value a newer guard might write that
// this daemon build predates).
func updateResultAlert(r update.Result) (Alert, bool) {
	if r.Notified {
		return Alert{}, false
	}
	base := Alert{Key: "update:result", Kind: "fire", Source: "update", Time: r.At}
	switch r.Outcome {
	case "committed":
		base.Title = "trinetra updated " + r.From + " → " + r.Version
		base.Severity = SevInfo
		return base, true
	case "rolled_back":
		base.Title = "trinetra update to " + r.Version + " failed its health check and was rolled back to " + r.From
		base.Body = r.Detail
		base.Severity = SevCritical
		return base, true
	default:
		return Alert{}, false
	}
}

// updateAvailableAlert reports the "update available" Alert for st, and
// whether one is due: st.Available is set and differs from
// st.AvailableNotified (so the same available version is never re-alerted,
// but a later, still-newer version -- or the same version becoming
// available again after an intervening notified one -- alerts again).
func updateAvailableAlert(st update.State) (Alert, bool) {
	if st.Available == "" || st.Available == st.AvailableNotified {
		return Alert{}, false
	}
	return Alert{
		Key:      "update:available",
		Title:    "trinetra " + st.Available + " is available (run: sudo trinetra update apply)",
		Severity: SevInfo,
		Kind:     "fire",
		Source:   "update",
		Time:     st.LastCheck,
	}, true
}

// updateStaleAlert reports the "no fresh signed release pointer" warning
// when lastPointerIssued (an RFC3339 timestamp, update.State.LastPointerIssued)
// is more than stalePointerAfter old, or reports false when it is unset
// (never fetched yet -- not evidence of staleness) or unparsable or fresh.
func updateStaleAlert(channel, lastPointerIssued string, now time.Time) (Alert, bool) {
	if lastPointerIssued == "" {
		return Alert{}, false
	}
	issued, err := time.Parse(time.RFC3339, lastPointerIssued)
	if err != nil {
		return Alert{}, false
	}
	if now.Sub(issued) <= stalePointerAfter {
		return Alert{}, false
	}
	return Alert{
		Key:      "update:stale",
		Title:    "no fresh signed release pointer for " + channel + " in over 14 days",
		Severity: SevWarning,
		Kind:     "fire",
		Source:   "update",
		Time:     now.Unix(),
	}, true
}

// startUpdateLoop is the daemon's self-update background loop (daemon.go):
// every updateLoopInterval it (1) notifies and marks Notified any
// not-yet-notified Last result, (2) runs a due update.channel check against
// a github source (errors are logged, not alerted, except that a newly
// stale pointer IS alerted -- once per continuous staleness episode, reset
// once a fresh pointer arrives, so a live daemon process is not paged every
// tick for a condition that has not changed), and (3) notifies a newly
// available version. It returns when ctx is done. Every state mutation
// re-reads state first (LoadState -> modify one field -> SaveState) so the
// CLI and the daemon never clobber each other's fields.
func startUpdateLoop(ctx context.Context, getCfg func() *config.Config, u updater, notify func(Alert)) {
	ticker := time.NewTicker(updateLoopInterval)
	defer ticker.Stop()
	staleNotified := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		updateLoopTick(ctx, u, getCfg(), notify, &staleNotified)
	}
}

// updateLoopTick runs one iteration of startUpdateLoop's body, split out so
// the three steps (each its own load/mutate/save) read clearly and so a
// future test could drive a single tick directly.
func updateLoopTick(ctx context.Context, u updater, c *config.Config, notify func(Alert), staleNotified *bool) {
	notifyPendingResult(u.paths, notify)
	runDueCheck(ctx, u, c, notify, staleNotified)
	notifyAvailable(u.paths, notify)
}

// notifyPendingResult delivers and marks Notified the current Last result,
// if any and not already notified.
func notifyPendingResult(p updatePaths, notify func(Alert)) {
	st, err := update.LoadState(p.dir())
	if err != nil || st.Last == nil || st.Last.Notified {
		return
	}
	a, ok := updateResultAlert(*st.Last)
	if !ok {
		return
	}
	notify(a)
	st2, err := update.LoadState(p.dir())
	if err != nil || st2.Last == nil {
		return
	}
	st2.Last.Notified = true
	if err := update.SaveState(p.dir(), st2); err != nil {
		log.Printf("update: mark result notified: %v", err)
	}
}

// checkRefreshedState reports whether a u.check(ctx, c) call that returned
// err actually reached and persisted a fresh LastCheck/LastPointerIssued
// (fix-round-1 F4): updater.check (update_cmd.go) only ever returns an error
// BEFORE touching state at all (channel off, no source, FetchLatest/
// FetchRelease/LoadState failed) -- UNLESS that error is one of
// update.CheckPolicy's sentinel errors (ErrAlreadyInstalled, ErrDowngrade,
// ErrTooOld, ErrWrongChannel), which check returns only AFTER it has already
// loaded, updated and saved state; a policy verdict is not a fetch/verify
// failure. A nil error is of course also success. Everything else (a
// network error, a bad signature, an expired/malformed pointer, an
// unreadable/unwritable state file) is a real failure: state was not
// refreshed this cycle, so the stale-pointer rule below must not evaluate
// against it as if it had been.
func checkRefreshedState(err error) bool {
	return err == nil ||
		errors.Is(err, update.ErrAlreadyInstalled) ||
		errors.Is(err, update.ErrDowngrade) ||
		errors.Is(err, update.ErrTooOld) ||
		errors.Is(err, update.ErrWrongChannel)
}

// runDueCheck runs u.check when update.channel is on, the source is github,
// and CheckInterval has elapsed since LastCheck, then evaluates the
// stale-pointer rule -- but ONLY when that check actually succeeded in
// talking to the source (checkRefreshedState; fix-round-1 F4): a check that
// truly failed (network error, bad signature, etc.) is logged only, exactly
// like any other check error, and must never itself trigger the
// stale-pointer alert just because state already looked stale from some
// earlier successful check.
func runDueCheck(ctx context.Context, u updater, c *config.Config, notify func(Alert), staleNotified *bool) {
	channel := c.UpdateChannel()
	if channel == "off" || c.UpdateSource() != "github" {
		return
	}
	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return
	}
	if st.LastCheck != 0 && time.Since(time.Unix(st.LastCheck, 0)) < c.UpdateCheckInterval() {
		return
	}
	_, checkErr := u.check(ctx, c)
	if checkErr != nil {
		log.Printf("update: check: %v", checkErr)
	}
	if !checkRefreshedState(checkErr) {
		return
	}

	st2, err := update.LoadState(u.paths.dir())
	if err != nil {
		return
	}
	if a, ok := updateStaleAlert(channel, st2.LastPointerIssued, u.clock()); ok {
		if !*staleNotified {
			notify(a)
			*staleNotified = true
		}
	} else {
		*staleNotified = false
	}
}

// notifyAvailable delivers and marks AvailableNotified the current Available
// version, if any and not already notified.
func notifyAvailable(p updatePaths, notify func(Alert)) {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return
	}
	a, ok := updateAvailableAlert(st)
	if !ok {
		return
	}
	notify(a)
	st2, err := update.LoadState(p.dir())
	if err != nil {
		return
	}
	st2.AvailableNotified = st.Available
	if err := update.SaveState(p.dir(), st2); err != nil {
		log.Printf("update: mark available notified: %v", err)
	}
}
