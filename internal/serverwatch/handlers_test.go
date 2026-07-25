package serverwatch

import (
	"strings"
	"testing"
	"time"
)

func TestHandleHelpAndStats(t *testing.T) {
	snap := Snapshot{CPU: 12, MemPct: 40, Online: true}
	if r := handleCommand("/help", nil, snap); !strings.Contains(r, "/stats") {
		t.Fatalf("help = %q", r)
	}
	if r := handleCommand("/stats", nil, snap); !strings.Contains(r, "CPU") {
		t.Fatalf("stats = %q", r)
	}
	if r := handleCommand("/gibberish", nil, snap); !strings.Contains(r, "/stats") {
		t.Fatalf("unknown should show help, got %q", r)
	}
}

func TestHandleDisk(t *testing.T) {
	snap := Snapshot{Disks: map[string]float64{"/": 42, "/boot": 10}, Online: true}
	r := handleCommand("/disk", nil, snap)
	if !strings.Contains(r, "/ 42%") {
		t.Fatalf("disk reply missing usage, got %q", r)
	}
	if strings.Contains(r, "internet") || strings.Contains(r, "DOWN") {
		t.Fatalf("disk reply must not fabricate connectivity, got %q", r)
	}
}

func TestHandleNet(t *testing.T) {
	if r := handleCommand("/net", nil, Snapshot{Online: true}); !strings.Contains(r, "up") {
		t.Fatalf("net up = %q", r)
	}
	if r := handleCommand("/net", nil, Snapshot{Online: false}); !strings.Contains(r, "DOWN") {
		t.Fatalf("net down = %q", r)
	}
}

// TestHandleHistoryFromStore verifies /history reads downtime events from the
// SampleStore (store.Events) rather than the legacy JSONL Store.
func TestHandleHistoryFromStore(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := newMemStore(StoreOptions{})
	end := now.Add(-2 * time.Hour)
	start := end.Add(-30 * time.Minute)
	if err := store.AppendEvent(DownEvent{
		Type:        "power_down",
		Start:       start.Unix(),
		End:         end.Unix(),
		DurationSec: int64(end.Sub(start) / time.Second),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	snap := Snapshot{TS: now.Unix()}
	r := handleCommand("/history 7", store, snap)
	if !strings.Contains(r, "power_down") {
		t.Fatalf("history should contain event, got %q", r)
	}
}

// TestHandleDownFromStore verifies /down (the same handler branch as
// /history) reads the downtime event via store.Events.
func TestHandleDownFromStore(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := newMemStore(StoreOptions{})
	end := now.Add(-1 * time.Hour)
	start := end.Add(-15 * time.Minute)
	if err := store.AppendEvent(DownEvent{
		Type:        "net_down",
		Start:       start.Unix(),
		End:         end.Unix(),
		DurationSec: int64(end.Sub(start) / time.Second),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	r := handleCommand("/down", store, Snapshot{TS: now.Unix()})
	if !strings.Contains(r, "net_down") {
		t.Fatalf("/down should contain event, got %q", r)
	}
}

// TestHandleHistoryNilStore verifies /history degrades to an empty reply
// (rather than panicking) when the SampleStore failed to open at startup.
func TestHandleHistoryNilStore(t *testing.T) {
	r := handleCommand("/history 7", nil, Snapshot{TS: time.Now().Unix()})
	if !strings.Contains(r, "no downtime") {
		t.Fatalf("nil-store history = %q, want a degrade-to-empty reply", r)
	}
}

func TestHandleDockerAndServices(t *testing.T) {
	snap := Snapshot{
		Containers:  map[string]string{"web": "running", "db": "exited"},
		FailedUnits: []string{"nginx.service"},
	}
	d := handleCommand("/docker", nil, snap)
	if !strings.Contains(d, "web") || !strings.Contains(d, "db") {
		t.Fatalf("/docker = %q", d)
	}
	s := handleCommand("/services", nil, snap)
	if !strings.Contains(s, "nginx.service") {
		t.Fatalf("/services = %q", s)
	}
	empty := handleCommand("/services", nil, Snapshot{})
	if !strings.Contains(empty, "no failed units") {
		t.Fatalf("/services empty = %q", empty)
	}
}

func TestInQuietHours(t *testing.T) {
	// window 23-8 wraps midnight
	at := func(h int) time.Time { return time.Date(2026, 7, 25, h, 0, 0, 0, time.UTC) }
	if !inQuietHours("23-8", at(2)) {
		t.Fatal("02:00 should be quiet")
	}
	if inQuietHours("23-8", at(12)) {
		t.Fatal("12:00 should not be quiet")
	}
	if !inQuietHours("23-8", at(23)) {
		t.Fatal("23:00 should be quiet")
	}
	if inQuietHours("", at(2)) {
		t.Fatal("empty spec disables quiet hours")
	}
}
