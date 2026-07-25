package serverwatch

import (
	"strings"
	"testing"
	"time"
)

func TestHandleHelpAndStats(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, fixedClock{time.Unix(1000, 0)})
	snap := Snapshot{CPU: 12, MemPct: 40, Online: true}
	if r := handleCommand("/help", st, snap); !strings.Contains(r, "/stats") {
		t.Fatalf("help = %q", r)
	}
	if r := handleCommand("/stats", st, snap); !strings.Contains(r, "CPU") {
		t.Fatalf("stats = %q", r)
	}
	if r := handleCommand("/gibberish", st, snap); !strings.Contains(r, "/stats") {
		t.Fatalf("unknown should show help, got %q", r)
	}
}

func TestHandleDisk(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, fixedClock{time.Unix(1000, 0)})
	snap := Snapshot{Disks: map[string]float64{"/": 42, "/boot": 10}, Online: true}
	r := handleCommand("/disk", st, snap)
	if !strings.Contains(r, "/ 42%") {
		t.Fatalf("disk reply missing usage, got %q", r)
	}
	if strings.Contains(r, "internet") || strings.Contains(r, "DOWN") {
		t.Fatalf("disk reply must not fabricate connectivity, got %q", r)
	}
}

func TestHandleNet(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir, fixedClock{time.Unix(1000, 0)})
	if r := handleCommand("/net", st, Snapshot{Online: true}); !strings.Contains(r, "up") {
		t.Fatalf("net up = %q", r)
	}
	if r := handleCommand("/net", st, Snapshot{Online: false}); !strings.Contains(r, "DOWN") {
		t.Fatalf("net down = %q", r)
	}
}

func TestHandleHistory(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	st := NewStore(dir, fixedClock{now})
	end := now.Add(-2 * time.Hour)
	start := end.Add(-30 * time.Minute)
	if err := st.AppendDown(DownEvent{
		Type:        "power_down",
		Start:       start.Unix(),
		End:         end.Unix(),
		DurationSec: int64(end.Sub(start) / time.Second),
	}); err != nil {
		t.Fatalf("AppendDown: %v", err)
	}
	snap := Snapshot{TS: now.Unix()}
	r := handleCommand("/history 7", st, snap)
	if !strings.Contains(r, "power_down") {
		t.Fatalf("history should contain event, got %q", r)
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
