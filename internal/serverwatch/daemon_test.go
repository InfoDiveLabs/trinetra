package serverwatch

import (
	"errors"
	"testing"
	"time"

	"serverwatch/internal/config"
)

func TestBuildChecksHonorsConfig(t *testing.T) {
	c := config.Default()
	_ = c.Set("thresholds.disk_pct", "80")
	c.SetTarget("disk:/boot", false) // disabled -> no check
	snap := Snapshot{
		CPU:    50,
		MemPct: 40,
		Disks:  map[string]float64{"/": 85, "/boot": 99},
	}
	checks := buildChecks(snap, c, nil)
	var haveRoot, haveBoot bool
	for _, ch := range checks {
		if ch.Key == "disk:/" {
			haveRoot = true
			if ch.Threshold != 80 || !ch.HasThreshold {
				t.Fatalf("root threshold = %v", ch.Threshold)
			}
		}
		if ch.Key == "disk:/boot" {
			haveBoot = true
		}
	}
	if !haveRoot {
		t.Fatal("expected a check for disk:/")
	}
	if haveBoot {
		t.Fatal("disabled target must not produce a check")
	}
}

func TestBuildChecksDiscovery(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		Containers:  map[string]string{"web": "running", "db": "exited"},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED", "/dev/sdb": "FAILED"},
	}
	active := map[string]ActiveAlert{"service:cron.service": {Since: 1}} // was failing, now recovered
	checks := buildChecks(snap, c, active)
	want := map[string]float64{
		"docker:web": 0, "docker:db": 1,
		"smart:/dev/sda": 0, "smart:/dev/sdb": 1,
		"service:nginx.service": 1, "service:cron.service": 0, // 0 => recovery
	}
	got := map[string]float64{}
	for _, ch := range checks {
		got[ch.Key] = ch.Value
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Fatalf("check %s = %v (present=%v), want %v", k, gv, ok, v)
		}
	}
}

func TestBuildChecksDockerSmartRecovery(t *testing.T) {
	c := config.Default()
	// Snapshot no longer contains "old-web" (container removed) nor "/dev/sdz"
	// (smartctl --scan started erroring / device gone). Their alerts are active.
	snap := Snapshot{
		Containers:  map[string]string{"web": "running"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED"},
	}
	active := map[string]ActiveAlert{
		"docker:old-web": {Since: 1},
		"smart:/dev/sdz": {Since: 1},
	}
	checks := buildChecks(snap, c, active)
	got := map[string]float64{}
	for _, ch := range checks {
		got[ch.Key] = ch.Value
	}
	// The vanished targets must synthesize a value=0 recovery check.
	if v, ok := got["docker:old-web"]; !ok || v != 0 {
		t.Fatalf("docker:old-web = %v present=%v, want recovery 0", v, ok)
	}
	if v, ok := got["smart:/dev/sdz"]; !ok || v != 0 {
		t.Fatalf("smart:/dev/sdz = %v present=%v, want recovery 0", v, ok)
	}
	// Still-present targets keep their normal check.
	if v, ok := got["docker:web"]; !ok || v != 0 {
		t.Fatalf("docker:web = %v present=%v", v, ok)
	}
}

func TestPingHealthchecks(t *testing.T) {
	called := ""
	pingHealthchecks("http://hc/abc", func(u string) error { called = u; return nil })
	if called != "http://hc/abc" {
		t.Fatalf("ping url = %q", called)
	}
	// empty url must be a no-op
	called = ""
	pingHealthchecks("", func(u string) error { called = u; return errors.New("x") })
	if called != "" {
		t.Fatal("empty url should not ping")
	}
}

func TestEventToAlert(t *testing.T) {
	now := int64(1700000000)

	critical := eventToAlert(Event{Key: "disk:/", Kind: "fire", Text: "disk:/ = 95.0 ≥ threshold 90.0", Critical: true}, now)
	if critical.Severity != SevCritical {
		t.Errorf("critical fire severity = %v, want SevCritical", critical.Severity)
	}
	if critical.Kind != "fire" {
		t.Errorf("kind = %q, want fire", critical.Kind)
	}
	if critical.Key != "disk:/" {
		t.Errorf("key = %q, want disk:/", critical.Key)
	}
	if critical.Title != "disk:/ = 95.0 ≥ threshold 90.0" {
		t.Errorf("title = %q", critical.Title)
	}
	if critical.Body != "" {
		t.Errorf("body = %q, want empty", critical.Body)
	}
	if critical.Source != "anomaly" {
		t.Errorf("source = %q, want anomaly", critical.Source)
	}
	if critical.Time != now {
		t.Errorf("time = %d, want %d", critical.Time, now)
	}

	warn := eventToAlert(Event{Key: "cpu", Kind: "fire", Text: "cpu high", Critical: false}, now)
	if warn.Severity != SevWarning {
		t.Errorf("non-critical fire severity = %v, want SevWarning", warn.Severity)
	}

	rec := eventToAlert(Event{Key: "cpu", Kind: "recover", Text: "cpu back to normal", Critical: false}, now)
	if rec.Kind != "recover" {
		t.Errorf("recover kind not preserved: %q", rec.Kind)
	}
	if rec.Severity != SevWarning {
		t.Errorf("recover non-critical severity = %v, want SevWarning", rec.Severity)
	}
}

// TestEventToAlertDispatchRespectsQuietHours exercises eventToAlert feeding
// straight into a Dispatcher built from fake, in-memory Channels (rather than
// going through config), asserting that quiet-hours gating is honored
// per-channel exactly like the direct Dispatch tests in notifier_test.go.
func TestEventToAlertDispatchRespectsQuietHours(t *testing.T) {
	always := &fakeNotifier{name: "critical-overrides-quiet"}
	quietAware := &fakeNotifier{name: "quiet-respecting"}
	channels := []Channel{
		{N: always, Route: Route{CriticalOverridesQuiet: true}, Enabled: true},
		{N: quietAware, Route: Route{}, Enabled: true},
	}
	d := NewDispatcher(channels, time.Second)

	now := int64(1700000000)
	critical := eventToAlert(Event{Key: "disk:/", Kind: "fire", Text: "disk:/ full", Critical: true}, now)
	warning := eventToAlert(Event{Key: "cpu", Kind: "fire", Text: "cpu high", Critical: false}, now)

	// During quiet hours: only the critical-overrides-quiet channel should
	// receive the critical alert; the warning must reach neither channel,
	// and the quiet-respecting channel must receive nothing at all.
	d.Dispatch(critical, true)
	d.Dispatch(warning, true)

	if got := len(always.received()); got != 1 {
		t.Errorf("critical-overrides-quiet channel received %d alerts during quiet hours, want 1", got)
	}
	if got := len(quietAware.received()); got != 0 {
		t.Errorf("quiet-respecting channel received %d alerts during quiet hours, want 0", got)
	}

	// Outside quiet hours the warning should now reach the quiet-respecting
	// channel too.
	d.Dispatch(warning, false)
	if got := len(quietAware.received()); got != 1 {
		t.Errorf("quiet-respecting channel received %d alerts outside quiet hours, want 1", got)
	}
}
