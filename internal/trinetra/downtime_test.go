package trinetra

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReconstructPowerDown(t *testing.T) {
	last := time.Unix(1000, 0)
	boot := time.Unix(5000, 0)
	ev, ok := reconstructPowerDown(last, boot, time.Time{}, false, 60*time.Second)
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
	if _, ok := reconstructPowerDown(last, boot, time.Time{}, false, 60*time.Second); ok {
		t.Fatal("90s gap should not be a downtime event")
	}
}

func TestNoPowerDownOnClockSkew(t *testing.T) {
	last := time.Unix(5000, 0)
	boot := time.Unix(1000, 0) // clock went backwards
	if _, ok := reconstructPowerDown(last, boot, time.Time{}, false, 60*time.Second); ok {
		t.Fatal("backwards clock must not emit a negative-duration event")
	}
}

func TestPowerDownExactlyAtThreshold(t *testing.T) {
	last := time.Unix(1000, 0)
	interval := 60 * time.Second
	// gap == 2*interval exactly -> NOT a downtime event.
	boot := time.Unix(1000+120, 0)
	if _, ok := reconstructPowerDown(last, boot, time.Time{}, false, interval); ok {
		t.Fatal("gap == 2*interval must not emit an event")
	}
	// Just over the threshold -> event with exact integer duration.
	bootOver := time.Unix(1000+121, 0)
	ev, ok := reconstructPowerDown(last, bootOver, time.Time{}, false, interval)
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
	if _, ok := reconstructPowerDown(last, boot, time.Time{}, false, 60*time.Second); ok {
		t.Fatal("boot == lastBeat must not emit an event")
	}
}

// TestNoPowerDownForMonitorRestart is the core #116 fix: a long heartbeat gap where the
// HOST stayed up.
func TestNoPowerDownForMonitorRestart(t *testing.T) {
	last := time.Unix(10_000, 0)
	daemonStart := time.Unix(14_000, 0) // 4000s gap (a crash-loop restart)
	hostBoot := time.Unix(500, 0)       // host booted long before lastBeat: up throughout
	if ev, ok := reconstructPowerDown(last, daemonStart, hostBoot, true, 60*time.Second); ok {
		t.Fatalf("host-up restart gap must not record downtime, got %+v", ev)
	}
}

// TestPowerDownWhenHostRebooted: the host actually rebooted during the gap, so it IS host
// downtime, and the window ends at the host boot instant (the real recovery).
func TestPowerDownWhenHostRebooted(t *testing.T) {
	last := time.Unix(10_000, 0)
	hostBoot := time.Unix(12_500, 0)    // host came back mid-gap
	daemonStart := time.Unix(13_000, 0) // daemon started a bit after boot
	ev, ok := reconstructPowerDown(last, daemonStart, hostBoot, true, 60*time.Second)
	if !ok {
		t.Fatal("a real host reboot during the gap must record a power_down")
	}
	if ev.Type != "power_down" || ev.Start != 10_000 || ev.End != 12_500 || ev.DurationSec != 2_500 {
		t.Fatalf("event = %+v, want end at host boot 12500 (duration 2500)", ev)
	}
}

func TestHostBootTimeParsesBtime(t *testing.T) {
	fs := fakeFS{files: map[string]string{
		"/proc/stat": "cpu  1 2 3 4\nbtime 1699999999\nprocesses 42\n",
	}}
	bt, ok := hostBootTime(fs)
	if !ok {
		t.Fatal("hostBootTime ok=false, want true for a well-formed /proc/stat")
	}
	if bt.Unix() != 1699999999 {
		t.Fatalf("hostBootTime = %d, want 1699999999", bt.Unix())
	}

	// No btime line -> not ok (caller falls back).
	fs2 := fakeFS{files: map[string]string{"/proc/stat": "cpu 1 2 3 4\n"}}
	if _, ok := hostBootTime(fs2); ok {
		t.Fatal("hostBootTime ok=true with no btime line, want false")
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

func TestShouldReportDowntimeDedupe(t *testing.T) {
	ev := DownEvent{Type: "power_down", Start: 100, End: 200, DurationSec: 100}
	if !shouldReportDowntime(ev, 0) {
		t.Fatal("first report should be allowed")
	}
	if shouldReportDowntime(ev, 200) {
		t.Fatal("already-reported window must not re-report")
	}
	if !shouldReportDowntime(DownEvent{End: 260}, 200) {
		t.Fatal("a newer window should report")
	}
}

func TestCleanStopRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clean-stop")
	now := time.Unix(1721900000, 0)
	if err := writeCleanStop(p, now); err != nil {
		t.Fatalf("writeCleanStop: %v", err)
	}
	got, ok := readCleanStop(p, osFS{})
	if !ok {
		t.Fatal("readCleanStop ok=false, want true")
	}
	if got != now.Unix() {
		t.Fatalf("got %d want %d", got, now.Unix())
	}
}

func TestReadCleanStopMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "does-not-exist")
	if _, ok := readCleanStop(p, osFS{}); ok {
		t.Fatal("readCleanStop on missing file should return ok=false")
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
