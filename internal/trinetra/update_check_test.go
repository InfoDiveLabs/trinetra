package trinetra

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
	"github.com/InfoDiveLabs/trinetra/internal/update/updatetest"
)

// pointerSource serves a signed <channel>.json naming version, issued at
// issued, plus a signed release for version on channel relChannel.
func pointerSource(t *testing.T, channel, version, relChannel string, issued time.Time) channelSource {
	t.Helper()
	ptr := update.Pointer{Schema: 1, Product: "trinetra", Channel: channel, Version: version,
		Issued: issued.Format(time.RFC3339), Expires: issued.Add(14 * 24 * time.Hour).Format(time.RFC3339)}
	pb, err := json.Marshal(ptr)
	if err != nil {
		t.Fatal(err)
	}
	rel := signedRelease(t, version, map[string][]byte{"trinetra-linux-amd64": []byte("x"), "trinetra-web-linux-amd64": []byte("y")})
	if relChannel != "stable" {
		m := update.Manifest{}
		json.Unmarshal(rel["manifest.json"], &m)
		m.Channel = relChannel
		mb, _ := json.Marshal(m)
		rel["manifest.json"] = mb
		rel["manifest.ci.sig"] = updatetest.NewTestSigner(1).SignRelease(mb)
		rel["manifest.maint.sig"] = updatetest.NewTestSigner(2).SignRelease(mb)
	}
	return channelSource{
		channel: map[string][]byte{channel + ".json": pb, channel + ".json.sig": updatetest.NewTestSigner(3).SignPointer(pb)},
		release: rel,
	}
}

// TestCheckSetsAvailableOnlyWhenPolicyPasses is R18: Available (which drives
// the "update available" alert, the web Apply button and Telegram /version)
// is set only for a release this host would actually accept -- right
// channel, above the floor, not known-bad -- and cleared otherwise.
func TestCheckSetsAvailableOnlyWhenPolicyPasses(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	issued := now.Add(-time.Hour)
	for _, c := range []struct {
		name, hostCh, relCh, running string
		bad                          bool
		want                         string
		wantErr                      error
	}{
		{"stable host, stable release", "stable", "stable", "0.4.1", false, "0.5.0", nil},
		{"beta host, stable release", "beta", "stable", "0.4.1", false, "0.5.0", nil},
		{"stable host, beta release", "stable", "beta", "0.4.1", false, "", update.ErrWrongChannel},
		{"already installed", "stable", "stable", "0.5.0", false, "", update.ErrAlreadyInstalled},
		{"known bad", "stable", "stable", "0.4.1", true, "", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := testUpdatePaths(t)
			st := update.State{Available: "0.4.9"}
			if c.bad {
				st.Bad = []string{"0.5.0"}
			}
			update.SaveState(p.dir(), st)
			cfg := config.Default()
			if err := cfg.Set("update.channel", c.hostCh); err != nil {
				t.Fatal(err)
			}
			u := updater{paths: p, keys: testKeys(), src: pointerSource(t, c.hostCh, "0.5.0", c.relCh, issued),
				now: func() time.Time { return now }, arch: "amd64", running: mustVer(c.running)}
			_, err := u.check(context.Background(), cfg)
			if c.wantErr != nil && !errors.Is(err, c.wantErr) {
				t.Fatalf("check err = %v, want %v", err, c.wantErr)
			}
			got, _ := update.LoadState(p.dir())
			if got.Available != c.want {
				t.Fatalf("Available = %q, want %q", got.Available, c.want)
			}
		})
	}
}

// TestUpdaterCheckUnparsableFloorFailsClosed is #138: a persisted floor that
// is valid JSON but not a valid version must refuse `trinetra update check`
// with a clear error instead of silently acting as "no floor" (which, with
// an unknown running version, would enforce no lower bound at all).
func TestUpdaterCheckUnparsableFloorFailsClosed(t *testing.T) {
	now := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	issued := now.Add(-time.Hour)
	p := testUpdatePaths(t)
	if err := update.SaveState(p.dir(), update.State{Floor: "not-a-version"}); err != nil {
		t.Fatalf("seed floor: %v", err)
	}
	cfg := config.Default()
	u := updater{paths: p, keys: testKeys(), src: pointerSource(t, "stable", "0.5.0", "stable", issued),
		now: func() time.Time { return now }, arch: "amd64", running: mustVer("0.4.1")}
	if _, err := u.check(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "not-a-version") {
		t.Fatalf("check() = %v, want an error naming the bad floor value", err)
	}
}
