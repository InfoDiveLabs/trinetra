package trinetra

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// recordingEngine builds an engine on the incident store at path whose clock is *now and
// records every delivered alert.
func recordingEngine(t *testing.T, path string, now *int64, mu *sync.Mutex, got *[]Alert) *fleetAlertEngine {
	t.Helper()
	store, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return newFleetAlertEngine(
		func() time.Time { mu.Lock(); defer mu.Unlock(); return time.Unix(*now, 0) },
		func(a Alert) bool { mu.Lock(); *got = append(*got, a); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		store,
	)
}

// A node goes down and comes back, then the master restarts: the restart must not re-send
// anything for the finished outage (it used to re-send the recovery under the down title).
func TestMasterRestartAfterShortOutageSendsNoStaleDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incidents.jsonl")
	var mu sync.Mutex
	var got []Alert
	now := int64(1000)

	e1 := recordingEngine(t, path, &now, &mu, &got)
	e1.Submit(alertSource{}, Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000})
	e1.waitIdleForTest()
	mu.Lock()
	now = 1010
	mu.Unlock()
	e1.Submit(alertSource{}, Alert{Key: "fleet:node:x:down", Title: "x is back", Severity: SevCritical, Kind: "recover", Time: 1010})
	e1.waitIdleForTest()
	for _, ts := range []int64{1040, 1070} {
		mu.Lock()
		now = ts
		mu.Unlock()
		e1.TickGrouping(time.Unix(ts, 0))
		e1.waitIdleForTest()
	}
	e1.Stop(time.Second)
	mu.Lock()
	before := append([]Alert(nil), got...)
	got = nil
	now = 1100
	mu.Unlock()

	e2 := recordingEngine(t, path, &now, &mu, &got)
	e2.waitIdleForTest()
	e2.TickGrouping(time.Unix(1100, 0))
	e2.waitIdleForTest()
	mu.Lock()
	after := append([]Alert(nil), got...)
	mu.Unlock()
	if len(before) != 2 || before[0].Kind != "fire" || before[1].Kind != "recover" {
		t.Fatalf("before restart: delivered %+v, want the down then the recovery", before)
	}
	if len(after) != 0 {
		t.Fatalf("after restart: %d stale notifications re-sent: %+v (before restart: %+v)", len(after), after, before)
	}
}
