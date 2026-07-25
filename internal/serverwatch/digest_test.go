package serverwatch

import (
	"testing"
	"time"
)

func TestMatchDaily(t *testing.T) {
	now := time.Date(2026, 7, 25, 9, 0, 30, 0, time.UTC)
	zero := time.Time{}
	if !matchDaily("09:00", now, zero) {
		t.Fatal("should fire at/after 09:00 when never run")
	}
	ranToday := time.Date(2026, 7, 25, 9, 0, 5, 0, time.UTC)
	if matchDaily("09:00", now, ranToday) {
		t.Fatal("should not fire twice same day")
	}
	early := time.Date(2026, 7, 25, 8, 59, 0, 0, time.UTC)
	if matchDaily("09:00", early, zero) {
		t.Fatal("should not fire before time")
	}
}

func TestBuildDailyDigest(t *testing.T) {
	s := buildDailyDigest(
		[]Sample{{CPU: 10}, {CPU: 80}, {CPU: 20}},
		[]DownEvent{{Type: "net_down", DurationSec: 120}},
	)
	if s == "" {
		t.Fatal("digest empty")
	}
}
