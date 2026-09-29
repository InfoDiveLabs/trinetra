// Package trinetra: update_daemon.go wires self-update into the running
// daemon: resuming a pending update at start (via resumePendingOnStart,
// update_guard.go) and the periodic loop that checks for a new release and
// turns update.State transitions into operator-facing Alerts ("update
// available", "updated"/"rolled back", and a stale-pointer warning).
package trinetra

import (
	"context"
	"errors"
	"fmt"
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

// freezeVerdict decides whether the channel looks frozen (R17), given the
// error (if any) of the check that just ran and the Issued time of the last
// pointer that verified (State.LastPointerIssued, RFC3339 or ""):
//
//   - an expired or missing pointer (or pointer signature) is the freeze
//     signature itself and counts at once;
//   - otherwise, a last good pointer issued more than stalePointerAfter ago
//     counts whatever the check's outcome, so an attacker who replays the
//     last valid pointer or blocks the channel is noticed within 14 days;
//   - a plain transport error with a recent (or no) last pointer does not.
//
// reason is the operator-facing explanation when stale is true.
func freezeVerdict(checkErr error, lastPointerIssued string, now time.Time) (stale bool, reason string) {
	if errors.Is(checkErr, update.ErrExpired) || errors.Is(checkErr, update.ErrNoPointer) {
		return true, checkErr.Error()
	}
	if lastPointerIssued == "" {
		return false, ""
	}
	issued, err := time.Parse(time.RFC3339, lastPointerIssued)
	if err != nil || now.Sub(issued) <= stalePointerAfter {
		return false, ""
	}
	days := int(now.Sub(issued) / (24 * time.Hour))
	reason = fmt.Sprintf("the last signed pointer was issued %d days ago", days)
	if checkErr != nil {
		reason += " (last check: " + checkErr.Error() + ")"
	}
	return true, reason
}

// updateStaleAlert is the freeze warning for channel with reason.
func updateStaleAlert(channel, reason string, now time.Time) Alert {
	return Alert{
		Key:      "update:stale",
		Title:    "no fresh signed release pointer for the " + channel + " channel: updates may be withheld",
		Body:     reason,
		Severity: SevWarning,
		Kind:     "fire",
		Source:   "update",
		Time:     now.Unix(),
	}
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		updateLoopTick(ctx, u, getCfg(), notify)
	}
}

// updateLoopTick runs one iteration of startUpdateLoop's body, split out so
// the three steps (each its own load/mutate/save) read clearly and so a
// future test could drive a single tick directly.
func updateLoopTick(ctx context.Context, u updater, c *config.Config, notify func(Alert)) {
	notifyPendingResult(u.paths, notify)
	runDueCheck(ctx, u, c, notify)
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
// err actually verified a fresh pointer: nil, or one of update.CheckPolicy's
// verdicts (or errKnownBad), which check returns only after it has fetched,
// verified and saved the pointer. Only such a check ends a stale episode.
func checkRefreshedState(err error) bool {
	return err == nil ||
		errors.Is(err, update.ErrAlreadyInstalled) ||
		errors.Is(err, update.ErrDowngrade) ||
		errors.Is(err, update.ErrTooOld) ||
		errors.Is(err, update.ErrWrongChannel) ||
		errors.Is(err, errKnownBad)
}

// runDueCheck runs u.check when update.channel is on, the source is github,
// and CheckInterval has elapsed since LastCheck, then applies the freeze
// rule (freezeVerdict, R17): a stale verdict alerts once per episode (the
// dedup, State.StaleNotified, is persisted so a restart does not re-page),
// and a check that verified a fresh pointer ends the episode. Other check
// errors are only logged.
func runDueCheck(ctx context.Context, u updater, c *config.Config, notify func(Alert)) {
	channel := c.UpdateChannel()
	if channel == "off" || c.UpdateSource() != "github" {
		return
	}
	st, err := update.LoadState(u.paths.dir())
	if err != nil {
		return
	}
	if st.LastCheck != 0 && u.clock().Sub(time.Unix(st.LastCheck, 0)) < c.UpdateCheckInterval() {
		return
	}
	_, checkErr := u.check(ctx, c)
	if checkErr != nil {
		log.Printf("update: check: %v", checkErr)
	}
	st2, err := update.LoadState(u.paths.dir())
	if err != nil {
		return
	}
	stale, reason := freezeVerdict(checkErr, st2.LastPointerIssued, u.clock())
	switch {
	case stale && !st2.StaleNotified:
		notify(updateStaleAlert(channel, reason, u.clock()))
		st2.StaleNotified = true
	case !stale && st2.StaleNotified && checkRefreshedState(checkErr):
		st2.StaleNotified = false
	default:
		return
	}
	if err := update.SaveState(u.paths.dir(), st2); err != nil {
		log.Printf("update: save stale-alert state: %v", err)
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
