package core

import "testing"

// staticEvents is a minimal EventsSource test double: events is returned
// verbatim from Events, ignoring the requested [from, to] range, mirroring
// internal/web/handlers_history_test.go's fakeEventsStore.
type staticEvents []DownEventView

func (s staticEvents) Events(from, to int64) ([]DownEventView, error) {
	return s, nil
}

func TestComputeAvailabilityOneOutage(t *testing.T) {
	// Reproduce the scenario from internal/web/availability_test.go: a single
	// net_down event of known duration inside the trailing 24h window.
	evs := staticEvents([]DownEventView{{Type: "net_down", Start: 1000, End: 1000 + 3600, DurationSec: 3600}})
	a := ComputeAvailability(evs, 1000+3600)
	if a.UptimePct <= 0 || a.UptimePct >= 100 {
		t.Fatalf("expected a partial uptime pct, got %v", a.UptimePct)
	}
}

// TestComputeAvailabilityCoalescesOverlap: overlapping same-type events count
// as ONE incident and their UNION of downtime, not the double-counted sum (#116).
func TestComputeAvailabilityCoalescesOverlap(t *testing.T) {
	now := int64(100000)
	evs := staticEvents([]DownEventView{
		{Type: "power_down", Start: now - 3600, End: now - 1800, DurationSec: 1800},
		{Type: "power_down", Start: now - 2400, End: now - 600, DurationSec: 1800}, // overlaps the first
	})
	a := ComputeAvailability(evs, now)
	if a.Incidents != 1 {
		t.Errorf("Incidents = %d, want 1 (overlapping events coalesced)", a.Incidents)
	}
	// Union downtime = (now-600) - (now-3600) = 3000s, not 1800+1800=3600.
	total := int64(24 * 60 * 60)
	wantPct := 100 * (1 - float64(3000)/float64(total))
	if diff := a.UptimePct - wantPct; diff > 0.001 || diff < -0.001 {
		t.Errorf("UptimePct = %v, want %v (union 3000s, not double-counted 3600s)", a.UptimePct, wantPct)
	}
}

func TestCoalesceDownEvents(t *testing.T) {
	// Different types never merge, even when overlapping.
	mixed := coalesceDownEvents([]DownEventView{
		{Type: "power_down", Start: 0, End: 100},
		{Type: "net_down", Start: 50, End: 150},
	})
	if len(mixed) != 2 {
		t.Errorf("different-type overlap merged: %+v", mixed)
	}
	// Non-overlapping same type stays separate.
	sep := coalesceDownEvents([]DownEventView{
		{Type: "power_down", Start: 0, End: 100},
		{Type: "power_down", Start: 200, End: 300},
	})
	if len(sep) != 2 {
		t.Errorf("disjoint same-type merged: %+v", sep)
	}
	// Touching windows (end == next start) merge into one.
	touch := coalesceDownEvents([]DownEventView{
		{Type: "power_down", Start: 200, End: 300},
		{Type: "power_down", Start: 100, End: 200},
	})
	if len(touch) != 1 || touch[0].Start != 100 || touch[0].End != 300 || touch[0].DurationSec != 200 {
		t.Errorf("touching windows did not coalesce to [100,300]: %+v", touch)
	}
}
