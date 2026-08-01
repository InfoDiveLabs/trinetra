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
