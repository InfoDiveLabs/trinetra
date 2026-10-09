// Package trinetra: update_daemon.go wires self-update into the running daemon.
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

// updateLoopInterval is how often startUpdateLoop wakes to notify a not-yet-notified
// result, run a due update.channel check, and notify a newly available version.
const updateLoopInterval = 5 * time.Minute

// updateLoopEvery is startUpdateLoop's tick: updateLoopInterval.
func updateLoopEvery() time.Duration {
	if d, ok := e2eUpdateLoopInterval(); ok {
		return d
	}
	return updateLoopInterval
}

// stalePointerAfter is how long with no fresh channel pointer (State's LastPointerIssued)
// is considered stale enough to warn about.
const stalePointerAfter = 14 * 24 * time.Hour

// updateResultAlert maps a not-yet-notified update.Result to the Alert startUpdateLoop
// delivers, and reports false for a Result that carries no alert.
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

// updateAvailableAlert reports the "update available" Alert for st, and whether one is due:
// st.Available is set and differs from st.AvailableNotified.
func updateAvailableAlert(st update.State, running update.Version) (Alert, bool) {
	avail := st.AvailableOver(running)
	if avail == "" || avail == st.AvailableNotified {
		return Alert{}, false
	}
	return Alert{
		Key:      "update:available",
		Title:    "trinetra " + avail + " is available (run: sudo trinetra update apply)",
		Severity: SevInfo,
		Kind:     "fire",
		Source:   "update",
		Time:     st.LastCheck,
	}, true
}

// freezeVerdict decides whether the channel looks frozen, given the error (if any) of the
// check that just ran and the Issued time of the last pointer that verified.
func freezeVerdict(checkErr error, lastPointerIssued string, now time.Time) (stale bool, reason string) {
	if lastPointerIssued == "" {
		return false, ""
	}
	if errors.Is(checkErr, update.ErrExpired) || errors.Is(checkErr, update.ErrNoPointer) {
		return true, checkErr.Error()
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

// startUpdateLoop is the daemon's self-update background loop (daemon.go): every
// updateLoopInterval it (1) notifies and marks Notified any not-yet-notified Last result.
func startUpdateLoop(ctx context.Context, getCfg func() *config.Config, u updater, notify func(Alert)) {
	ticker := time.NewTicker(updateLoopEvery())
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

// updateLoopTick runs one iteration of startUpdateLoop's body, split out so the three
// steps.
func updateLoopTick(ctx context.Context, u updater, c *config.Config, notify func(Alert)) {
	notifyPendingResult(u.paths, notify)
	notifyRestoreFailed(u.paths, u.clock(), notify)
	runDueCheck(ctx, u, c, notify)
	notifyAvailable(u.paths, notify)
}

// notifyPendingResult delivers queued update outcomes and drops only those it delivered, so
// one a guard records meanwhile waits for the next tick.
func notifyPendingResult(p updatePaths, notify func(Alert)) {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return
	}
	queue := st.Unnotified
	if len(queue) == 0 && st.Last != nil && !st.Last.Notified {
		queue = []update.Result{*st.Last}
	}
	if len(queue) == 0 {
		return
	}
	for _, r := range queue {
		if a, ok := updateResultAlert(r); ok {
			notify(a)
		}
	}
	err = update.WithState(p.dir(), func(st2 *update.State) error {
		keep := st2.Unnotified[:0]
		for _, r := range st2.Unnotified {
			if !resultIn(r, queue) {
				keep = append(keep, r)
			}
		}
		st2.Unnotified = keep
		if len(st2.Unnotified) == 0 {
			st2.Unnotified = nil
		}
		if st2.Last != nil && resultIn(*st2.Last, queue) {
			st2.Last.Notified = true
		}
		return nil
	})
	if err != nil {
		log.Printf("update: mark result notified: %v", err)
	}
}

func resultIn(r update.Result, rs []update.Result) bool {
	for _, x := range rs {
		if x.At == r.At && x.Version == r.Version && x.From == r.From && x.Outcome == r.Outcome {
			return true
		}
	}
	return false
}

// notifyRestoreFailed delivers and marks RestoreFailedNotified the current Pending's
// restore failure, if any and not already notified.
func notifyRestoreFailed(p updatePaths, now time.Time, notify func(Alert)) {
	st, err := update.LoadState(p.dir())
	if err != nil || st.Pending == nil || st.Pending.RestoreFailed == "" || st.Pending.RestoreFailedNotified {
		return
	}
	pending := *st.Pending
	notify(Alert{
		Key:      "update:restore_failed",
		Title:    "trinetra update to " + pending.Version + " failed its health check, and restoring " + pending.From + " also failed",
		Body:     pending.RestoreFailed + "; the update watchdog will keep retrying",
		Severity: SevCritical,
		Kind:     "fire",
		Source:   "update",
		Time:     now.Unix(),
	})
	err = update.WithState(p.dir(), func(st2 *update.State) error {
		if st2.Pending != nil && st2.Pending.RestoreFailed != "" {
			st2.Pending.RestoreFailedNotified = true
		}
		return nil
	})
	if err != nil {
		log.Printf("update: mark restore-failed notified: %v", err)
	}
}

// checkRefreshedState reports whether a u.check(ctx, c) call that returned err actually
// verified a fresh pointer: nil, or one of update.CheckPolicy's verdicts (or errKnownBad).
func checkRefreshedState(err error) bool {
	return err == nil ||
		errors.Is(err, update.ErrAlreadyInstalled) ||
		errors.Is(err, update.ErrDowngrade) ||
		errors.Is(err, update.ErrTooOld) ||
		errors.Is(err, update.ErrWrongChannel) ||
		errors.Is(err, errKnownBad)
}

// runDueCheck runs u.check when update.channel is on, the source is github, and
// CheckInterval has elapsed since LastCheck, then applies the freeze rule.
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
	everVerified := st.LastPointerIssued != ""
	_, checkErr := u.check(ctx, c)
	switch {
	case checkErr == nil:
		// nothing to log
	case checkRefreshedState(checkErr) || everVerified:
		// Either a fresh pointer just verified fine (checkErr is only a policy verdict -- already
		// installed, downgrade, wrong channel, known bad -- not a freeze/configuration problem).
		log.Printf("update: check: %v", checkErr)
	case checkErr.Error() != st.LastCheckError:
		// never verified a pointer yet, and this check didn't either
		// -- log the cause once, not every tick, until it changes.
		log.Printf("update: updates not configured: %v", checkErr)
	}
	var alert *Alert
	err = update.WithState(u.paths.dir(), func(st2 *update.State) error {
		if checkRefreshedState(checkErr) {
			st2.LastCheckError = ""
		} else if checkErr != nil {
			st2.LastCheckError = checkErr.Error()
		}
		stale, reason := freezeVerdict(checkErr, st2.LastPointerIssued, u.clock())
		switch {
		case stale && !st2.StaleNotified:
			a := updateStaleAlert(channel, reason, u.clock())
			alert = &a
			st2.StaleNotified = true
		case !stale && st2.StaleNotified && checkRefreshedState(checkErr):
			st2.StaleNotified = false
		}
		return nil
	})
	if err != nil {
		log.Printf("update: save stale-alert state: %v", err)
		return
	}
	if alert != nil {
		notify(*alert)
	}
}

// notifyAvailable delivers and marks AvailableNotified the current Available
// version, if any and not already notified.
func notifyAvailable(p updatePaths, notify func(Alert)) {
	st, err := update.LoadState(p.dir())
	if err != nil {
		return
	}
	a, ok := updateAvailableAlert(st, runningVersion())
	if !ok {
		return
	}
	notify(a)
	err = update.WithState(p.dir(), func(st2 *update.State) error {
		st2.AvailableNotified = st.Available
		return nil
	})
	if err != nil {
		log.Printf("update: mark available notified: %v", err)
	}
}
