//go:build web

package web

import (
	"fmt"
	"testing"
)

// TestComputeAvailabilityNoEventsIsFullyUp pins Part 2's baseline case: an
// empty (or nil) downtime event store must report 100% up, 0 incidents, and
// every block up -- the strip's "nothing has ever gone down" state.
func TestComputeAvailabilityNoEventsIsFullyUp(t *testing.T) {
	now := int64(1_700_100_000)

	for _, store := range []EventsStore{nil, &fakeEventsStore{events: nil}} {
		av := ComputeAvailability(store, now)
		if len(av.Blocks) != 96 {
			t.Fatalf("len(Blocks) = %d, want 96", len(av.Blocks))
		}
		for i, b := range av.Blocks {
			if b.Down {
				t.Fatalf("block %d Down = true, want all-up with no events", i)
			}
		}
		if av.UptimePct != 100 {
			t.Fatalf("UptimePct = %v, want 100", av.UptimePct)
		}
		if av.Incidents != 0 {
			t.Fatalf("Incidents = %d, want 0", av.Incidents)
		}
		if av.DowntimeStr != "0m" {
			t.Fatalf("DowntimeStr = %q, want %q", av.DowntimeStr, "0m")
		}
	}
}

// TestComputeAvailabilityMarksDownBlocksAndSummarizes pins the core
// obligation: given fake downtime events inside the last 24h, the resulting
// strip data classifies exactly the overlapping 15-min blocks as down,
// counts one incident per distinct event, and computes an exact uptime%/
// total-downtime figure from the events' real overlap durations (not
// block-quantized).
func TestComputeAvailabilityMarksDownBlocksAndSummarizes(t *testing.T) {
	const day = 86400
	now := int64(1_700_100_000)
	from := now - day

	// Event 1: exactly blocks 8-9 (30 min, 2h-2.5h into the window).
	e1Start, e1End := from+7200, from+9000
	// Event 2: 5 minutes inside block 55 only (~13.9h into the window).
	e2Start, e2End := from+50000, from+50300

	store := &fakeEventsStore{events: []DownEventView{
		{Type: "power_down", Start: e1Start, End: e1End, DurationSec: e1End - e1Start},
		{Type: "net_down", Start: e2Start, End: e2End, DurationSec: e2End - e2Start},
	}}

	av := ComputeAvailability(store, now)
	if len(av.Blocks) != 96 {
		t.Fatalf("len(Blocks) = %d, want 96", len(av.Blocks))
	}

	wantDown := map[int]bool{8: true, 9: true, 55: true}
	for i, b := range av.Blocks {
		if b.Down != wantDown[i] {
			t.Errorf("block %d Down = %v, want %v", i, b.Down, wantDown[i])
		}
	}

	if av.Incidents != 2 {
		t.Fatalf("Incidents = %d, want 2", av.Incidents)
	}
	// Total downtime = 1800s + 300s = 2100s = 35m exactly.
	if av.DowntimeStr != "35m" {
		t.Fatalf("DowntimeStr = %q, want %q", av.DowntimeStr, "35m")
	}
	// uptime% = 100*(1 - 2100/86400) = 97.569444...%
	if got := fmt.Sprintf("%.2f", av.UptimePct); got != "97.57" {
		t.Fatalf("UptimePct = %s, want 97.57", got)
	}
}

// TestComputeAvailabilityClipsEventsToWindow confirms an event that started
// before the 24h window (e.g. an outage that's still ongoing, or one that
// began earlier and only partly falls in-window) only counts its in-window
// portion toward downtime/uptime%, not its full real-world duration.
func TestComputeAvailabilityClipsEventsToWindow(t *testing.T) {
	const day = 86400
	now := int64(1_700_100_000)
	from := now - day

	// Started 1h before the window, ends 1h into it: only 1h (3600s) of the
	// full 2h event actually falls inside [from, now].
	store := &fakeEventsStore{events: []DownEventView{
		{Type: "power_down", Start: from - 3600, End: from + 3600, DurationSec: 7200},
	}}

	av := ComputeAvailability(store, now)
	if av.Incidents != 1 {
		t.Fatalf("Incidents = %d, want 1", av.Incidents)
	}
	if av.DowntimeStr != "1h 0m" {
		t.Fatalf("DowntimeStr = %q, want %q (clipped to the in-window 1h, not the full 2h)", av.DowntimeStr, "1h 0m")
	}
	// Block 0 (the window's very first block) must be down.
	if !av.Blocks[0].Down {
		t.Fatal("block 0 should be down (event overlaps the window's start)")
	}
}
