package serverwatch

import (
	"testing"
	"time"
)

func TestReconstructPowerDown(t *testing.T) {
	last := time.Unix(1000, 0)
	boot := time.Unix(5000, 0)
	ev, ok := reconstructPowerDown(last, boot, 60*time.Second)
	if !ok {
		t.Fatal("expected an event for a 4000s gap")
	}
	if ev.Type != "power_down" || ev.Start != 1000 || ev.End != 5000 || ev.DurationSec != 4000 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestNoPowerDownForShortBlip(t *testing.T) {
	last := time.Unix(1000, 0)
	boot := time.Unix(1090, 0) // 90s <= 2*60s
	if _, ok := reconstructPowerDown(last, boot, 60*time.Second); ok {
		t.Fatal("90s gap should not be a downtime event")
	}
}

func TestNoPowerDownOnClockSkew(t *testing.T) {
	last := time.Unix(5000, 0)
	boot := time.Unix(1000, 0) // clock went backwards
	if _, ok := reconstructPowerDown(last, boot, 60*time.Second); ok {
		t.Fatal("backwards clock must not emit a negative-duration event")
	}
}

func TestNetTrackerOpenClose(t *testing.T) {
	var n NetTracker
	if _, closed := n.Update(false, 100); closed {
		t.Fatal("going offline should not close an interval")
	}
	n.Update(false, 160) // still offline
	ev, closed := n.Update(true, 300)
	if !closed || ev.Type != "net_down" || ev.Start != 100 || ev.End != 300 || ev.DurationSec != 200 {
		t.Fatalf("closed=%v ev=%+v", closed, ev)
	}
	// Back online again, no double-close.
	if _, closed := n.Update(true, 400); closed {
		t.Fatal("should not close twice")
	}
}
