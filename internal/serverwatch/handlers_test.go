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
