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

func TestBuildChecksSetsIntervalPerTier(t *testing.T) {
	c := config.Default()
	c.FastInterval = 5
	c.SampleInterval = 60
	snap := Snapshot{
		CPU:     50,
		MemPct:  40,
		SwapPct: 1,
		TempC:   55,
		Disks:   map[string]float64{"/": 85},
	}
	checks := buildChecks(snap, c, nil)
	got := map[string]int{}
	for _, ch := range checks {
		got[ch.Key] = ch.Interval
	}
	for _, key := range []string{"cpu", "mem", "swap", "temp"} {
		if got[key] != 5 {
			t.Fatalf("%s.Interval = %d, want fast_interval 5", key, got[key])
		}
	}
	if got["disk:/"] != 60 {
		t.Fatalf("disk:/.Interval = %d, want sample_interval 60", got["disk:/"])
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

func TestBuildFastChecksOnlyCheapMetrics(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		CPU:         50,
		MemPct:      40,
		SwapPct:     1,
		TempC:       55,
		Disks:       map[string]float64{"/": 85},
		Containers:  map[string]string{"web": "running"},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED"},
	}
	checks := buildFastChecks(snap, c)
	got := map[string]bool{}
	for _, ch := range checks {
		got[ch.Key] = true
	}
	want := []string{"cpu", "mem", "swap", "temp"}
	if len(got) != len(want) {
		t.Fatalf("buildFastChecks returned %d checks (%v), want exactly %v", len(got), keysOf(got), want)
	}
	for _, k := range want {
		if !got[k] {
			t.Errorf("buildFastChecks missing %q", k)
		}
	}
	for _, ch := range checks {
		if ch.Interval != c.FastInterval {
			t.Errorf("%s.Interval = %d, want fast_interval %d", ch.Key, ch.Interval, c.FastInterval)
		}
	}
}

func TestBuildSlowChecksOnlyExpensiveMetrics(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		CPU:         50, // must be ignored by buildSlowChecks
		MemPct:      40,
		Disks:       map[string]float64{"/": 85},
		Containers:  map[string]string{"web": "running"},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED"},
	}
	active := map[string]ActiveAlert{"service:cron.service": {Since: 1}}
	checks := buildSlowChecks(snap, c, active)
	got := map[string]bool{}
	for _, ch := range checks {
		got[ch.Key] = true
		if ch.Key == "cpu" || ch.Key == "mem" || ch.Key == "swap" || ch.Key == "temp" {
			t.Fatalf("buildSlowChecks must not emit fast-tier key %q", ch.Key)
		}
		if ch.Interval != c.SampleInterval {
			t.Errorf("%s.Interval = %d, want sample_interval %d", ch.Key, ch.Interval, c.SampleInterval)
		}
	}
	for _, k := range []string{"disk:/", "docker:web", "service:nginx.service", "service:cron.service", "smart:/dev/sda"} {
		if !got[k] {
			t.Errorf("buildSlowChecks missing %q", k)
		}
	}
}

func keysOf(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestFastSlowUnionMatchesBuildChecks guards against a check silently
// dropping out of both tiers during future edits: the union of
// buildFastChecks and buildSlowChecks must always contain exactly the same
// keys as the combined buildChecks (kept as a thin fast+slow wrapper).
func TestFastSlowUnionMatchesBuildChecks(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		CPU: 50, MemPct: 40, SwapPct: 1, TempC: 55,
		Disks:       map[string]float64{"/": 85, "/boot": 10},
		Containers:  map[string]string{"web": "running", "db": "exited"},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED", "/dev/sdb": "FAILED"},
	}
	active := map[string]ActiveAlert{
		"service:cron.service": {Since: 1},
		"docker:old-web":       {Since: 1},
		"smart:/dev/sdz":       {Since: 1},
	}
	keySet := func(cs []Check) map[string]bool {
		m := map[string]bool{}
		for _, c := range cs {
			m[c.Key] = true
		}
		return m
	}
	union := keySet(buildFastChecks(snap, c))
	for k := range keySet(buildSlowChecks(snap, c, active)) {
		union[k] = true
	}
	all := keySet(buildChecks(snap, c, active))
	if len(union) != len(all) {
		t.Fatalf("fast+slow union = %v (%d keys), buildChecks = %v (%d keys)", keysOf(union), len(union), keysOf(all), len(all))
	}
	for k := range all {
		if !union[k] {
			t.Errorf("buildChecks key %q missing from fast+slow union", k)
		}
	}
}

func TestShouldHeartbeat(t *testing.T) {
	cases := []struct {
		name        string
		last, now   int64
		intervalSec int
		want        bool
	}{
		{"before interval elapsed", 100, 129, 30, false},
		{"exactly at interval", 100, 130, 30, true},
		{"after interval", 100, 200, 30, true},
		{"first heartbeat ever (last=0 sentinel)", 0, 1700000000, 30, true},
		{"zero interval always heartbeats", 100, 101, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldHeartbeat(tc.last, tc.now, tc.intervalSec); got != tc.want {
				t.Errorf("shouldHeartbeat(%d, %d, %d) = %v, want %v", tc.last, tc.now, tc.intervalSec, got, tc.want)
			}
		})
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

func TestSlowEvery(t *testing.T) {
	cases := []struct {
		fast, slow, want int
	}{
		{5, 60, 12}, // default config: 12 fast ticks per slow tick
		{60, 60, 1}, // equal intervals: slow tier every tick
		{10, 65, 6}, // non-multiple: floor division, still exact-ish
		{60, 30, 1}, // slow < fast: guard to minimum of 1
		{0, 60, 1},  // guard against fast<=0 (would divide by zero)
		{-5, 60, 1}, // guard against negative fast
	}
	for _, c := range cases {
		if got := slowEvery(c.fast, c.slow); got != c.want {
			t.Errorf("slowEvery(%d, %d) = %d, want %d", c.fast, c.slow, got, c.want)
		}
	}
}

func TestCollectFastPopulatesCheapFields(t *testing.T) {
	fs := fakeFS{
		files: map[string]string{
			"/proc/meminfo": "MemTotal:       1000 kB\nMemAvailable:    500 kB\nSwapTotal:       200 kB\nSwapFree:        100 kB\n",
			"/proc/loadavg": "1.50 1.00 0.50 1/200 1234",
		},
	}
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		t.Fatalf("collectFast must not exec external commands, got %q", name)
		return nil, nil
	}}
	var prev CPUStat
	snap := collectFast(x, fs, &prev)

	if snap.MemPct != 50 {
		t.Errorf("MemPct = %v, want 50", snap.MemPct)
	}
	if snap.SwapPct != 50 {
		t.Errorf("SwapPct = %v, want 50", snap.SwapPct)
	}
	if snap.Load1 != 1.5 {
		t.Errorf("Load1 = %v, want 1.5", snap.Load1)
	}
	// The slow-tier fields must NOT be touched by collectFast.
	if snap.Disks != nil {
		t.Errorf("collectFast populated Disks: %+v, want nil", snap.Disks)
	}
	if snap.Containers != nil {
		t.Errorf("collectFast populated Containers: %+v, want nil", snap.Containers)
	}
}

func TestCollectSlowPopulatesExpensiveFields(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "df":
			return []byte("Filesystem 1B-blocks Used Available Capacity Mounted\n/dev/sda1 100 90 10 90% /\n"), nil
		case "docker":
			return []byte("web\trunning\tUp 3 hours\n"), nil
		case "systemctl":
			return []byte("nginx.service loaded failed failed A high performance web server\n"), nil
		case "smartctl":
			if len(args) > 0 && args[0] == "--scan" {
				return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
			}
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: true, method: "socket"}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	snap := collectSlow(x, fs, da)

	if snap.Disks["/"] != 90 {
		t.Errorf("Disks[/] = %v, want 90", snap.Disks["/"])
	}
	if snap.Containers["web"] != "running" {
		t.Errorf("Containers[web] = %q, want running", snap.Containers["web"])
	}
	if len(snap.FailedUnits) != 1 || snap.FailedUnits[0] != "nginx.service" {
		t.Errorf("FailedUnits = %+v", snap.FailedUnits)
	}
	if snap.SmartHealth["/dev/sda"] != "PASSED" {
		t.Errorf("SmartHealth[/dev/sda] = %q, want PASSED", snap.SmartHealth["/dev/sda"])
	}
	if !snap.Online {
		t.Error("Online = false, want true (fake dial always succeeds)")
	}
	if snap.DockerAccess != "socket" {
		t.Errorf("DockerAccess = %q, want socket", snap.DockerAccess)
	}
	// The fast-tier fields must NOT be touched by collectSlow.
	if snap.CPU != 0 || snap.MemPct != 0 || snap.Load1 != 0 {
		t.Errorf("collectSlow populated fast fields: cpu=%v mem=%v load1=%v", snap.CPU, snap.MemPct, snap.Load1)
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

func TestFastMetricSet(t *testing.T) {
	snap := Snapshot{CPU: 12, MemPct: 34, SwapPct: 5, Load1: 1.5, TempC: 60}
	ms := fastMetricSet(snap)
	want := MetricSet{"cpu": 12, "mem": 34, "swap": 5, "load1": 1.5, "temp": 60}
	if len(ms) != len(want) {
		t.Fatalf("fastMetricSet = %+v, want %+v", ms, want)
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("fastMetricSet[%q] = %v, want %v", k, ms[k], v)
		}
	}
}

func TestFastMetricSetOmitsTempWhenZero(t *testing.T) {
	snap := Snapshot{CPU: 12, MemPct: 34, SwapPct: 5, Load1: 1.5, TempC: 0}
	ms := fastMetricSet(snap)
	if _, ok := ms["temp"]; ok {
		t.Fatalf("fastMetricSet with TempC=0 = %+v, want no \"temp\" key", ms)
	}
	if len(ms) != 4 {
		t.Fatalf("fastMetricSet = %+v, want exactly cpu/mem/swap/load1", ms)
	}
}

func TestSlowMetricSet(t *testing.T) {
	snap := Snapshot{Disks: map[string]float64{"/": 40, "/boot": 12}}
	ms := slowMetricSet(snap)
	want := MetricSet{"disk:/": 40, "disk:/boot": 12}
	if len(ms) != len(want) {
		t.Fatalf("slowMetricSet = %+v, want %+v", ms, want)
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("slowMetricSet[%q] = %v, want %v", k, ms[k], v)
		}
	}
}

func TestSlowMetricSetNoDisks(t *testing.T) {
	ms := slowMetricSet(Snapshot{})
	if len(ms) != 0 {
		t.Fatalf("slowMetricSet with no disks = %+v, want empty", ms)
	}
}

func TestSamplerMetricSetsWriteThroughToStore(t *testing.T) {
	// Integration-style: exercise the actual write path a fast/slow tick
	// takes — fastMetricSet/slowMetricSet feeding SampleStore.Append — against
	// a real (memory) backend, then read it back via Query.
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	snap := Snapshot{
		TS:      1700000000,
		CPU:     42,
		MemPct:  55,
		SwapPct: 2,
		Load1:   0.8,
		TempC:   50,
		Disks:   map[string]float64{"/": 70},
	}
	if err := store.Append(snap.TS, fastMetricSet(snap)); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(snap.TS, slowMetricSet(snap)); err != nil {
		t.Fatal(err)
	}

	cpuPts, err := store.Query("cpu", snap.TS, snap.TS, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cpuPts) != 1 || cpuPts[0].Avg != 42 {
		t.Fatalf("cpu query = %+v, want one point avg 42", cpuPts)
	}

	diskPts, err := store.Query("disk:/", snap.TS, snap.TS, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(diskPts) != 1 || diskPts[0].Avg != 70 {
		t.Fatalf("disk:/ query = %+v, want one point avg 70", diskPts)
	}
}
