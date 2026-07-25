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

func TestMatchWeekly(t *testing.T) {
	zero := time.Time{}
	monday := time.Date(2026, 7, 20, 9, 0, 30, 0, time.UTC)  // Monday
	tuesday := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)  // Tuesday
	prevMonday := time.Date(2026, 7, 13, 9, 0, 5, 0, time.UTC) // previous Monday

	if !matchWeekly("mon@09:00", monday, zero) {
		t.Fatal("should fire on matching weekday at/after time when never run")
	}
	if matchWeekly("mon@09:00", tuesday, zero) {
		t.Fatal("should not fire on non-matching weekday")
	}
	ranSameMonday := time.Date(2026, 7, 20, 9, 0, 5, 0, time.UTC)
	if matchWeekly("mon@09:00", monday, ranSameMonday) {
		t.Fatal("should not fire twice same day")
	}
	if !matchWeekly("mon@09:00", monday, prevMonday) {
		t.Fatal("should re-fire on a new week after last week's run")
	}
	if !matchWeekly("MON@09:00", monday, zero) {
		t.Fatal("weekday match should be case-insensitive")
	}
	if matchWeekly("09:00", monday, zero) {
		t.Fatal("spec without @ should not fire")
	}
	if matchWeekly("monday@09:00", monday, zero) {
		t.Fatal("unknown/full-name weekday token should not fire")
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
