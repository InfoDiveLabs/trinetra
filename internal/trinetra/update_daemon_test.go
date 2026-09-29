package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
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

// TestRunDueCheckAlertsStaleOnceWhenCheckSucceeds is fix-round-1 F4: the
// stale-pointer rule must fire when the pointer really is stale (>14 days
// since LastPointerIssued) as long as the check that observed it actually
// succeeded talking to the source -- here, a channel pointer whose signed
// Expires is right at its 14-day-since-Issued ceiling (VerifyPointer,
// internal/update/verify.go) but not yet past it, so FetchLatest verifies it
// cleanly even though it is already stale by this package's own
// stalePointerAfter threshold. The release itself resolves to
// update.ErrAlreadyInstalled (Version == Floor == Running): an everyday,
// non-fatal CheckPolicy outcome that must still count as "the check
// succeeded" for the stale-pointer rule (this is in fact the realistic
// steady-state case: a healthy host that is already up to date, whose
// upstream channel has simply stopped publishing fresh pointers).
func TestRunDueCheckAlertsStaleOnceWhenCheckSucceeds(t *testing.T) {
	p := testUpdatePaths(t)
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	issued := now.Add(-14*24*time.Hour - 30*time.Minute)
	expires := issued.Add(14*24*time.Hour + time.Hour) // now + 30min: not yet expired
	ptr := update.Pointer{Schema: 1, Product: "trinetra", Channel: "stable", Version: "0.4.1",
		Issued: issued.Format(time.RFC3339), Expires: expires.Format(time.RFC3339)}
	pb, err := json.Marshal(ptr)
	if err != nil {
		t.Fatal(err)
	}
	src := channelSource{
		channel: map[string][]byte{
			"stable.json":     pb,
			"stable.json.sig": update.NewTestSigner(3).SignPointer(pb),
		},
		release: signedRelease(t, "0.4.1", map[string][]byte{"trinetra-linux-amd64": []byte("x")}),
	}
	u := updater{paths: p, keys: testKeys(), src: src, now: func() time.Time { return now },
		arch: "amd64", running: mustVer("0.4.1")}
	c := config.Default()

	var notified []Alert
	staleNotified := false
	runDueCheck(context.Background(), u, c, func(a Alert) { notified = append(notified, a) }, &staleNotified)

	if len(notified) != 1 || notified[0].Key != "update:stale" {
		t.Fatalf("notified=%+v", notified)
	}
	if !staleNotified {
		t.Fatal("staleNotified not set")
	}
	st, _ := update.LoadState(p.dir())
	if st.LastPointerIssued != issued.Format(time.RFC3339) {
		t.Fatalf("state not refreshed by the successful check: %+v", st)
	}

	// A second due check with the same (still stale) pointer must not
	// re-alert.
	notified = nil
	runDueCheck(context.Background(), u, c, func(a Alert) { notified = append(notified, a) }, &staleNotified)
	if len(notified) != 0 {
		t.Fatalf("re-alerted an unchanged stale pointer: %+v", notified)
	}
}

// TestRunDueCheckDoesNotAlertStaleOnCheckFailure is fix-round-1 F4's other
// half: a check that fails outright (network error, bad signature, no
// channel pointer at all -- here, mapSource.ChannelAsset's unconditional
// update.ErrNoChannel) must never trigger the stale-pointer alert, even
// when the state it leaves behind already looks stale from a much earlier
// successful check. Checks are logged, not alerted, on failure; the
// exception is a STALE pointer observed by a SUCCEEDING check, not a
// failing one.
func TestRunDueCheckDoesNotAlertStaleOnCheckFailure(t *testing.T) {
	p := testUpdatePaths(t)
	stale := time.Now().Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	if err := update.SaveState(p.dir(), update.State{LastPointerIssued: stale}); err != nil {
		t.Fatal(err)
	}
	u := updater{paths: p, keys: testKeys(), src: mapSource{}, now: time.Now}
	c := config.Default()

	var notified []Alert
	staleNotified := false
	runDueCheck(context.Background(), u, c, func(a Alert) { notified = append(notified, a) }, &staleNotified)

	if len(notified) != 0 {
		t.Fatalf("alerted stale on a failed check: %+v", notified)
	}
	if staleNotified {
		t.Fatal("staleNotified set on a failed check")
	}
}
