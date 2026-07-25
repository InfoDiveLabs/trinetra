package serverwatch

import (
	"os"
	"path/filepath"
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

func TestPowerDownExactlyAtThreshold(t *testing.T) {
	last := time.Unix(1000, 0)
	interval := 60 * time.Second
	// gap == 2*interval exactly -> NOT a downtime event.
	boot := time.Unix(1000+120, 0)
	if _, ok := reconstructPowerDown(last, boot, interval); ok {
		t.Fatal("gap == 2*interval must not emit an event")
	}
	// Just over the threshold -> event with exact integer duration.
	bootOver := time.Unix(1000+121, 0)
	ev, ok := reconstructPowerDown(last, bootOver, interval)
	if !ok {
		t.Fatal("gap just over 2*interval should emit an event")
	}
	if ev.Type != "power_down" || ev.Start != 1000 || ev.End != 1121 || ev.DurationSec != 121 {
		t.Fatalf("event = %+v", ev)
	}
}

func TestNoPowerDownWhenBootEqualsLastBeat(t *testing.T) {
	last := time.Unix(1000, 0)
	boot := time.Unix(1000, 0) // boot == lastBeat, zero gap
	if _, ok := reconstructPowerDown(last, boot, 60*time.Second); ok {
		t.Fatal("boot == lastBeat must not emit an event")
	}
}

func TestHeartbeatRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")
	now := time.Unix(1721900000, 0)
	if err := writeHeartbeat(p, now); err != nil {
		t.Fatalf("writeHeartbeat: %v", err)
	}
	got, ok := readHeartbeat(p, osFS{})
	if !ok {
		t.Fatal("readHeartbeat ok=false, want true")
	}
	if got.Unix() != now.Unix() {
		t.Fatalf("got Unix=%d want %d", got.Unix(), now.Unix())
	}
}

func TestReadHeartbeatMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "does-not-exist")
	if _, ok := readHeartbeat(p, osFS{}); ok {
		t.Fatal("readHeartbeat on missing file should return ok=false")
	}
}

func TestReadHeartbeatCorrupt(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "heartbeat")
	if err := os.WriteFile(p, []byte("notanumber"), 0o644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	if _, ok := readHeartbeat(p, osFS{}); ok {
		t.Fatal("readHeartbeat on corrupt content should return ok=false")
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
