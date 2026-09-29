package trinetra

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
