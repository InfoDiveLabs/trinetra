package core

import (
	"testing"
	"time"
)

// TestNextMaintenanceOccurrenceUpcoming covers a window that hasn't started yet today:
// NextMaintenanceOccurrence must return its next start, not something from a later week.
func TestNextMaintenanceOccurrenceUpcoming(t *testing.T) {
	// 2024-01-01 is a Monday.
	now := time.Date(2024, 1, 1, 6, 0, 0, 0, time.UTC)
	m := Maintenance{Weekdays: []int{int(time.Monday)}, From: "22:00", To: "23:00", TZ: "UTC"}
	occ, ok := NextMaintenanceOccurrence(m, now)
	if !ok {
		t.Fatalf("NextMaintenanceOccurrence: ok = false, want true")
	}
	want := time.Date(2024, 1, 1, 22, 0, 0, 0, time.UTC).Unix()
	if occ.Start != want {
		t.Errorf("Start = %d, want %d (today 22:00)", occ.Start, want)
	}
}

// TestNextMaintenanceOccurrenceCurrentlyActive covers a window that is active RIGHT NOW:
// NextMaintenanceOccurrence must return that occurrence.
func TestNextMaintenanceOccurrenceCurrentlyActive(t *testing.T) {
	now := time.Date(2024, 1, 1, 22, 30, 0, 0, time.UTC) // Monday, mid-window
	m := Maintenance{Weekdays: []int{int(time.Monday)}, From: "22:00", To: "23:00", TZ: "UTC"}
	occ, ok := NextMaintenanceOccurrence(m, now)
	if !ok {
		t.Fatalf("NextMaintenanceOccurrence: ok = false, want true")
	}
	wantStart := time.Date(2024, 1, 1, 22, 0, 0, 0, time.UTC).Unix()
	wantEnd := time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC).Unix()
	if occ.Start != wantStart || occ.End != wantEnd {
		t.Errorf("occ = %+v, want [%d,%d) (the currently active occurrence)", occ, wantStart, wantEnd)
	}
}

// TestNextMaintenanceOccurrenceNextWeek covers a window whose day already passed this week:
// the next occurrence must land 7 days later, not be missing.
func TestNextMaintenanceOccurrenceNextWeek(t *testing.T) {
	now := time.Date(2024, 1, 2, 6, 0, 0, 0, time.UTC) // Tuesday, after Monday's window ended
	m := Maintenance{Weekdays: []int{int(time.Monday)}, From: "22:00", To: "23:00", TZ: "UTC"}
	occ, ok := NextMaintenanceOccurrence(m, now)
	if !ok {
		t.Fatalf("NextMaintenanceOccurrence: ok = false, want true")
	}
	want := time.Date(2024, 1, 8, 22, 0, 0, 0, time.UTC).Unix() // next Monday
	if occ.Start != want {
		t.Errorf("Start = %d, want %d (next Monday)", occ.Start, want)
	}
}

// TestNextMaintenanceOccurrenceBadTZ covers an unparsable TZ (should never happen for
// anything SaveMaintenance already validated and stored).
func TestNextMaintenanceOccurrenceBadTZ(t *testing.T) {
	m := Maintenance{Weekdays: []int{1}, From: "22:00", To: "23:00", TZ: "Not/AZone"}
	if _, ok := NextMaintenanceOccurrence(m, time.Now()); ok {
		t.Errorf("ok = true for an unparsable TZ, want false")
	}
}

// TestMaintenanceOccurrencesCrossesMidnight covers a window crossing midnight (From > To
// lexicographically): its END must be computed as start+duration.
func TestMaintenanceOccurrencesCrossesMidnight(t *testing.T) {
	m := Maintenance{Weekdays: []int{int(time.Sunday)}, From: "22:00", To: "02:00", TZ: "UTC"}
	from := time.Date(2024, 1, 7, 0, 0, 0, 0, time.UTC) // a Sunday
	occs := MaintenanceOccurrences(m, from, from.Add(24*time.Hour))
	if len(occs) != 1 {
		t.Fatalf("occurrences = %+v, want exactly 1", occs)
	}
	start := time.Unix(occs[0].Start, 0).UTC()
	end := time.Unix(occs[0].End, 0).UTC()
	if start.Weekday() != time.Sunday {
		t.Errorf("start weekday = %v, want Sunday", start.Weekday())
	}
	if end.Sub(start) != time.Hour*4 {
		t.Errorf("duration = %v, want 4h", end.Sub(start))
	}
}
