package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

func TestUpdateResultAlert(t *testing.T) {
	a, ok := updateResultAlert(update.Result{Version: "0.5.0", From: "0.4.1", Outcome: "rolled_back", Detail: "reported 0.4.1"})
	if !ok || a.Severity != SevCritical || !strings.Contains(a.Title, "rolled back") {
		t.Fatalf("%+v %v", a, ok)
	}
	a, ok = updateResultAlert(update.Result{Version: "0.5.0", From: "0.4.1", Outcome: "committed"})
	if !ok || a.Severity != SevInfo || !strings.Contains(a.Title, "0.4.1 → 0.5.0") {
		t.Fatalf("%+v %v", a, ok)
	}
	if _, ok := updateResultAlert(update.Result{Outcome: "committed", Notified: true}); ok {
		t.Fatal("notified result re-alerted")
	}
}

// TestNotifyRestoreFailedAlertsOnce is issue #136's alerting half:
// rollbackPending keeps Pending set (with RestoreFailed recorded) when
// restoring the previous build fails, since the guard is a separate,
// short-lived process that cannot notify directly. notifyRestoreFailed is
// the daemon's periodic-loop chance to raise the critical alert exactly
// once for that failure episode, deduped via the persisted
// RestoreFailedNotified flag -- repeated failing watchdog retries (each a
// fresh guard run that rewrites RestoreFailed) must not re-alert every
// tick.
func TestNotifyRestoreFailedAlertsOnce(t *testing.T) {
	p := testUpdatePaths(t)
	now := time.Unix(5000, 0)
	pending := update.Pending{Version: "0.5.0", From: "0.4.1", RestoreFailed: "restoring the previous build also failed: permission denied"}
	if err := update.SaveState(p.dir(), update.State{Pending: &pending}); err != nil {
		t.Fatal(err)
	}

	var got []Alert
	notifyRestoreFailed(p, now, func(a Alert) { got = append(got, a) })
	if len(got) != 1 {
		t.Fatalf("alerts = %d, want 1: %+v", len(got), got)
	}
	if got[0].Severity != SevCritical {
		t.Fatalf("severity = %v, want critical: %+v", got[0].Severity, got[0])
	}
	if !strings.Contains(got[0].Title, "0.5.0") || !strings.Contains(got[0].Body, "permission denied") {
		t.Fatalf("alert does not explain the failure: %+v", got[0])
	}

	st, err := update.LoadState(p.dir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || !st.Pending.RestoreFailedNotified {
		t.Fatalf("RestoreFailedNotified not persisted: %+v", st.Pending)
	}

	// A repeated failing retry: the guard reran, restorePrevious failed
	// again, and rewrote Pending.RestoreFailed -- but left
	// RestoreFailedNotified as it was (true). No second alert.
	st.Pending.RestoreFailed = "restoring the previous build also failed: permission denied (retry)"
	if err := update.SaveState(p.dir(), st); err != nil {
		t.Fatal(err)
	}
	got = nil
	notifyRestoreFailed(p, now, func(a Alert) { got = append(got, a) })
	if len(got) != 0 {
		t.Fatalf("re-alerted a still-failing retry: %+v", got)
	}
}

// TestNotifyRestoreFailedNoPending covers the common case (nothing to do):
// no Pending, or a Pending with no restore failure, must not alert.
func TestNotifyRestoreFailedNoPending(t *testing.T) {
	p := testUpdatePaths(t)
	var got []Alert
	notifyRestoreFailed(p, time.Unix(1, 0), func(a Alert) { got = append(got, a) })
	if len(got) != 0 {
		t.Fatalf("alerted with no state at all: %+v", got)
	}

	pending := update.Pending{Version: "0.5.0", From: "0.4.1"}
	if err := update.SaveState(p.dir(), update.State{Pending: &pending}); err != nil {
		t.Fatal(err)
	}
	notifyRestoreFailed(p, time.Unix(1, 0), func(a Alert) { got = append(got, a) })
	if len(got) != 0 {
		t.Fatalf("alerted for a Pending with no restore failure: %+v", got)
	}
}

func TestUpdateAvailableAlertOncePerVersion(t *testing.T) {
	if _, ok := updateAvailableAlert(update.State{Available: "0.5.0"}); !ok {
		t.Fatal("no alert for a new available version")
	}
	if _, ok := updateAvailableAlert(update.State{Available: "0.5.0", AvailableNotified: "0.5.0"}); ok {
		t.Fatal("re-alerted the same available version")
	}
}

// channelSource is a Source fake for runDueCheck tests: unlike mapSource
// (update_apply_test.go), whose ChannelAsset always returns
// update.ErrNoChannel, this also serves channel pointer bytes so a full
// u.check() round trip (FetchLatest + FetchRelease) can succeed.
type channelSource struct {
	channel map[string][]byte
	release mapSource
}

func (s channelSource) ReleaseAsset(ctx context.Context, version, name string) (io.ReadCloser, error) {
	return s.release.ReleaseAsset(ctx, version, name)
}

func (s channelSource) ChannelAsset(ctx context.Context, name string) (io.ReadCloser, error) {
	b, ok := s.channel[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// errSource fails every fetch with err (a network outage, say).
type errSource struct{ err error }

func (e errSource) ReleaseAsset(context.Context, string, string) (io.ReadCloser, error) {
	return nil, e.err
}
func (e errSource) ChannelAsset(context.Context, string) (io.ReadCloser, error) { return nil, e.err }

// signedPointer builds a stable pointer naming 0.4.1 with the given
// issued/expires, signed by the trusted test pointer key.
func signedPointer(t *testing.T, issued, expires time.Time) (pb, sig []byte) {
	t.Helper()
	ptr := update.Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.4.1",
		Issued: issued.Format(time.RFC3339), Expires: expires.Format(time.RFC3339)}
	pb, err := json.Marshal(ptr)
	if err != nil {
		t.Fatal(err)
	}
	return pb, updatetest.NewTestSigner(3).SignPointer(pb)
}

func staleAlerts(as []Alert) int {
	n := 0
	for _, a := range as {
		if a.Key == "update:stale" {
			n++
		}
	}
	return n
}

// TestRunDueCheckFreezeDetection is R17: the freeze alert must actually be
// able to fire. An expired or missing pointer is the freeze signature and
// alerts at once; a LastPointerIssued older than 14 days alerts whatever the
// check's outcome; a plain network error alerts only once the last good
// pointer is more than 14 days old. Each episode alerts once, and the dedup
// is persisted in State so a daemon restart does not re-page.
func TestRunDueCheckFreezeDetection(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	old := now.Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	release := signedRelease(t, "0.4.1", map[string][]byte{"trinetra-linux-amd64": []byte("x"), "trinetra-web-linux-amd64": []byte("y")})

	expiredPB, expiredSig := signedPointer(t, now.Add(-15*24*time.Hour), now.Add(-24*time.Hour))
	nearPB, nearSig := signedPointer(t, now.Add(-14*24*time.Hour-30*time.Minute), now.Add(30*time.Minute))
	okPB, okSig := signedPointer(t, now.Add(-time.Hour), now.Add(13*24*time.Hour))

	for _, c := range []struct {
		name      string
		lastIssue string
		src       update.Source
		want      int
	}{
		{"expired pointer alerts at once", fresh,
			channelSource{channel: map[string][]byte{"stable.json": expiredPB, "stable.json.sig": expiredSig}, release: release}, 1},
		{"missing pointer alerts at once", fresh, channelSource{channel: map[string][]byte{}, release: release}, 1},
		{"missing pointer signature alerts at once", fresh,
			channelSource{channel: map[string][]byte{"stable.json": okPB}, release: release}, 1},
		{"valid pointer issued over 14 days ago alerts", "",
			channelSource{channel: map[string][]byte{"stable.json": nearPB, "stable.json.sig": nearSig}, release: release}, 1},
		{"network error with a fresh last pointer does not alert", fresh, errSource{errors.New("dial tcp: connection refused")}, 0},
		{"network error with no pointer ever seen does not alert", "", errSource{errors.New("dial tcp: connection refused")}, 0},
		{"network error 14+ days after the last good pointer alerts", old, errSource{errors.New("dial tcp: connection refused")}, 1},
		{"fresh valid pointer does not alert", old,
			channelSource{channel: map[string][]byte{"stable.json": okPB, "stable.json.sig": okSig}, release: release}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testUpdatePaths(t)
			update.SaveState(p.dir(), update.State{LastPointerIssued: c.lastIssue})
			u := updater{paths: p, keys: testKeys(), src: c.src, now: func() time.Time { return now },
				arch: "amd64", running: mustVer("0.4.1")}
			var got []Alert
			runDueCheck(context.Background(), u, config.Default(), func(a Alert) { got = append(got, a) })
			if n := staleAlerts(got); n != c.want {
				t.Fatalf("stale alerts = %d, want %d (%+v)", n, c.want, got)
			}
			if c.want == 0 {
				return
			}
			// Same episode, next due check (and, since the dedup lives in
			// State, as if after a daemon restart): no second alert.
			st, _ := update.LoadState(p.dir())
			if !st.StaleNotified {
				t.Fatalf("dedup not persisted: %+v", st)
			}
			st.LastCheck = 0
			update.SaveState(p.dir(), st)
			got = nil
			runDueCheck(context.Background(), u, config.Default(), func(a Alert) { got = append(got, a) })
			if n := staleAlerts(got); n != 0 {
				t.Fatalf("re-alerted the same episode: %+v", got)
			}
		})
	}
}

// TestRunDueCheckFreshPointerEndsEpisode: once a fresh pointer verifies, the
// persisted dedup resets, so the next freeze alerts again.
func TestRunDueCheckFreshPointerEndsEpisode(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{StaleNotified: true})
	okPB, okSig := signedPointer(t, now.Add(-time.Hour), now.Add(13*24*time.Hour))
	release := signedRelease(t, "0.4.1", map[string][]byte{"trinetra-linux-amd64": []byte("x"), "trinetra-web-linux-amd64": []byte("y")})
	u := updater{paths: p, keys: testKeys(), now: func() time.Time { return now }, arch: "amd64", running: mustVer("0.4.1"),
		src: channelSource{channel: map[string][]byte{"stable.json": okPB, "stable.json.sig": okSig}, release: release}}
	runDueCheck(context.Background(), u, config.Default(), func(Alert) {})
	if st, _ := update.LoadState(p.dir()); st.StaleNotified {
		t.Fatalf("fresh pointer did not end the stale episode: %+v", st)
	}
}

// captureLog redirects the standard logger's output to a buffer for the rest
// of the test, restoring it on cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// TestRunDueCheckNeverVerifiedPointer is R25: freeze detection only makes
// sense once a host has verified a channel pointer at least once
// (State.LastPointerIssued). A fresh, unconfigured install (update.source=
// github, private release repo, no update.github_token) 404s on every
// check; before any pointer has ever verified, that must not raise the
// stale/freeze alert on tick one, and must not spam a log line every tick --
// just one line per distinct failure cause, and the cause surfaces via
// State.LastCheckError (what `update status` reads).
func TestRunDueCheckNeverVerifiedPointer(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	p := testUpdatePaths(t)
	u := updater{paths: p, keys: testKeys(), src: channelSource{channel: map[string][]byte{}}, now: func() time.Time { return now },
		arch: "amd64", running: mustVer("0.4.1")}

	logs := captureLog(t)
	for i := 0; i < 3; i++ {
		var got []Alert
		runDueCheck(context.Background(), u, config.Default(), func(a Alert) { got = append(got, a) })
		if n := staleAlerts(got); n != 0 {
			t.Fatalf("loop %d: alerted before any pointer was ever verified: %+v", i, got)
		}
	}

	st, err := update.LoadState(p.dir())
	if err != nil {
		t.Fatal(err)
	}
	if st.StaleNotified {
		t.Fatalf("StaleNotified set though no pointer has ever verified: %+v", st)
	}
	if st.LastCheckError == "" {
		t.Fatalf("no reason recorded for `update status` to show: %+v", st)
	}
	if n := strings.Count(logs.String(), "updates not configured:"); n != 1 {
		t.Fatalf(`logged "updates not configured:" %d times across 3 loops with the same cause, want 1: %s`, n, logs.String())
	}
}

// TestRunDueCheckSeenOnceThenExpired: once a pointer has verified once
// (State.LastPointerIssued set), R17 applies unchanged -- an expired pointer
// is the freeze signature and alerts at once, unlike
// TestRunDueCheckNeverVerifiedPointer's silence before that.
func TestRunDueCheckSeenOnceThenExpired(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{LastPointerIssued: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)})
	release := signedRelease(t, "0.4.1", map[string][]byte{"trinetra-linux-amd64": []byte("x"), "trinetra-web-linux-amd64": []byte("y")})
	expiredPB, expiredSig := signedPointer(t, now.Add(-15*24*time.Hour), now.Add(-24*time.Hour))
	u := updater{paths: p, keys: testKeys(), now: func() time.Time { return now }, arch: "amd64", running: mustVer("0.4.1"),
		src: channelSource{channel: map[string][]byte{"stable.json": expiredPB, "stable.json.sig": expiredSig}, release: release}}

	var got []Alert
	runDueCheck(context.Background(), u, config.Default(), func(a Alert) { got = append(got, a) })
	if n := staleAlerts(got); n != 1 {
		t.Fatalf("stale alerts = %d, want 1: %+v", n, got)
	}
}

// TestRunDueCheckSeenOnceThenMissing: once a pointer has verified once, a
// 404/missing pointer (e.g. the token was removed afterward) still counts as
// a real freeze signal and alerts, same as TestRunDueCheckFreezeDetection's
// "missing pointer alerts at once" case.
func TestRunDueCheckSeenOnceThenMissing(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	p := testUpdatePaths(t)
	update.SaveState(p.dir(), update.State{LastPointerIssued: now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)})
	u := updater{paths: p, keys: testKeys(), now: func() time.Time { return now }, arch: "amd64", running: mustVer("0.4.1"),
		src: channelSource{channel: map[string][]byte{}}}

	var got []Alert
	runDueCheck(context.Background(), u, config.Default(), func(a Alert) { got = append(got, a) })
	if n := staleAlerts(got); n != 1 {
		t.Fatalf("stale alerts = %d, want 1 (token removed after a pointer was once seen must still alert): %+v", n, got)
	}
}
