package serverwatch

import (
	"errors"
	"strings"
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
	if snap.Load5 != 1.0 {
		t.Errorf("Load5 = %v, want 1.0", snap.Load5)
	}
	if snap.Load15 != 0.5 {
		t.Errorf("Load15 = %v, want 0.5", snap.Load15)
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
			if len(args) > 0 && args[0] == "stats" {
				return []byte("web\t3.00%\t100MiB / 1GiB\t1MB / 1MB\n"), nil
			}
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

	snap := collectSlow(x, fs, da, config.Default(), nil, 0)

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
	// collect.container_stats defaults to true and docker is available -> populated.
	if snap.ContainerStats["web"].CPUPct != 3 {
		t.Errorf("ContainerStats[web].CPUPct = %v, want 3", snap.ContainerStats["web"].CPUPct)
	}
	if snap.ContainerStats["web"].MemMiB != 100 {
		t.Errorf("ContainerStats[web].MemMiB = %v, want 100", snap.ContainerStats["web"].MemMiB)
	}
	// The fast-tier fields must NOT be touched by collectSlow.
	if snap.CPU != 0 || snap.MemPct != 0 || snap.Load1 != 0 {
		t.Errorf("collectSlow populated fast fields: cpu=%v mem=%v load1=%v", snap.CPU, snap.MemPct, snap.Load1)
	}
}

// TestCollectSlowPopulatesDiskDetailAndSmartAttrs asserts collectSlow merges
// `df -PT -B1` (device/fstype/usage/size) with `df -Pi` (inode%) into
// snap.DiskDetail keyed by mount, alongside snap.Disks, and fills
// snap.SmartAttrs from `smartctl -A <dev>` for every discovered SMART
// device (alongside snap.SmartHealth).
func TestCollectSlowPopulatesDiskDetailAndSmartAttrs(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "df" && len(args) > 0 && args[0] == "-PT":
			return []byte("Filesystem Type 1-blocks Used Available Capacity Mounted on\n" +
				"/dev/sda1 ext4 100 90 10 90% /\n"), nil
		case name == "df" && len(args) > 0 && args[0] == "-Pi":
			return []byte("Filesystem Inodes IUsed IFree IUse% Mounted on\n" +
				"/dev/sda1 1000 700 300 70% /\n"), nil
		case name == "df":
			return []byte("Filesystem 1B-blocks Used Available Capacity Mounted\n/dev/sda1 100 90 10 90% /\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "--scan":
			return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-H":
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-A":
			return []byte("ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE\n" +
				"194 Temperature_Celsius     0x0022   109   095   000    Old_age   Always       -       37\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	snap := collectSlow(x, fs, da, config.Default(), nil, 0)

	if snap.Disks["/"] != 90 {
		t.Fatalf("Disks[/] = %v, want 90 (unchanged)", snap.Disks["/"])
	}
	dd, ok := snap.DiskDetail["/"]
	if !ok {
		t.Fatalf("DiskDetail missing mount /: %+v", snap.DiskDetail)
	}
	if dd.Device != "/dev/sda1" || dd.FsType != "ext4" {
		t.Errorf("DiskDetail[/] device/fstype = %q/%q, want /dev/sda1/ext4", dd.Device, dd.FsType)
	}
	if dd.UsagePct != 90 || dd.FreeBytes != 10 || dd.SizeBytes != 100 {
		t.Errorf("DiskDetail[/] = %+v", dd)
	}
	if dd.InodePct != 70 {
		t.Errorf("DiskDetail[/].InodePct = %v, want 70", dd.InodePct)
	}
	if dd.DaysToFullKnown {
		t.Errorf("DiskDetail[/].DaysToFullKnown = true, want false (nil store)")
	}
	if snap.SmartAttrs["/dev/sda"].TempC != 37 {
		t.Errorf("SmartAttrs[/dev/sda].TempC = %v, want 37", snap.SmartAttrs["/dev/sda"].TempC)
	}
}

// TestCollectSlowSkipsSmartAttrsWhenDisabled asserts collect.smart_attrs=false
// suppresses the `smartctl -A <dev>` call (the fake Exec fails the test if
// it's requested) while the cheaper `--scan`/`-H` health check still runs and
// still populates snap.SmartHealth.
func TestCollectSlowSkipsSmartAttrsWhenDisabled(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "smartctl" && len(args) > 0 && args[0] == "--scan":
			return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-H":
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-A":
			t.Fatal("smartctl -A must not be called when collect.smart_attrs is disabled")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	c := config.Default()
	if err := c.Set("collect.smart_attrs", "false"); err != nil {
		t.Fatal(err)
	}
	snap := collectSlow(x, fs, da, c, nil, 0)
	if snap.SmartAttrs != nil {
		t.Errorf("SmartAttrs = %+v, want nil when collect.smart_attrs disabled", snap.SmartAttrs)
	}
	if snap.SmartHealth["/dev/sda"] != "PASSED" {
		t.Errorf("SmartHealth[/dev/sda] = %q, want PASSED (health check unaffected)", snap.SmartHealth["/dev/sda"])
	}
}

// TestCollectSlowSkipsSmartAttrsOnNilConfig mirrors the container-stats/units
// nil-config guard: collectSlow must not panic (or call `smartctl -A`) with
// a nil *config.Config.
func TestCollectSlowSkipsSmartAttrsOnNilConfig(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "smartctl" && len(args) > 0 && args[0] == "--scan":
			return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-H":
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-A":
			t.Fatal("smartctl -A must not be called with a nil config")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	snap := collectSlow(x, fs, da, nil, nil, 0)
	if snap.SmartAttrs != nil {
		t.Errorf("SmartAttrs = %+v, want nil with nil config", snap.SmartAttrs)
	}
}

// TestCollectSlowDiskDetailProjectsDaysToFull asserts that when a store IS
// supplied, collectSlow fills DiskDetail.DaysToFull/DaysToFullKnown from a
// rising "disk:<mount>" history in that store, exercising the
// projectMountDaysToFull wiring end-to-end (not just projectDaysToFull in
// isolation).
func TestCollectSlowDiskDetailProjectsDaysToFull(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "df" && len(args) > 0 && args[0] == "-PT":
			return []byte("Filesystem Type 1-blocks Used Available Capacity Mounted on\n" +
				"/dev/sda1 ext4 100 90 10 90% /\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	store := newMemStore(StoreOptions{})
	const day = 86400
	const nowUnix = 10 * day
	// A rising history for "disk:/" over the last few days: 2%/day.
	for i := 0; i <= 5; i++ {
		ts := nowUnix - int64((5-i)*day)
		pct := 80 + float64(i)*2
		_ = store.Append(ts, MetricSet{"disk:/": pct})
	}

	snap := collectSlow(x, fs, da, config.Default(), store, nowUnix)

	dd, ok := snap.DiskDetail["/"]
	if !ok {
		t.Fatalf("DiskDetail missing mount /: %+v", snap.DiskDetail)
	}
	if !dd.DaysToFullKnown {
		t.Fatalf("DaysToFullKnown = false, want true for a rising history")
	}
	if dd.DaysToFull <= 0 {
		t.Errorf("DaysToFull = %v, want > 0", dd.DaysToFull)
	}
}

// TestSmartMetricSet asserts smartMetricSet emits one "smart:<dev>:temp"
// entry per device with a known (>0) temperature, and omits devices whose
// attribute set didn't report one.
func TestSmartMetricSet(t *testing.T) {
	snap := Snapshot{SmartAttrs: map[string]SmartAttr{
		"/dev/sda": {TempC: 37, WearPct: 5, ReallocSectors: 2},
		"/dev/sdb": {TempC: 0},
	}}
	ms := smartMetricSet(snap)
	want := MetricSet{"smart:/dev/sda:temp": 37}
	if len(ms) != len(want) {
		t.Fatalf("smartMetricSet = %+v, want %+v", ms, want)
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("smartMetricSet[%q] = %v, want %v", k, ms[k], v)
		}
	}
}

func TestSmartMetricSetEmpty(t *testing.T) {
	if ms := smartMetricSet(Snapshot{}); len(ms) != 0 {
		t.Fatalf("smartMetricSet(empty) = %+v, want empty", ms)
	}
}

// TestCollectSlowSkipsContainerStatsWhenDisabled asserts the
// collect.container_stats=false opt-out actually suppresses the
// `docker stats` call: the fake Exec fails the test if "stats" is
// requested, so ContainerStats staying nil is proof the call was skipped.
func TestCollectSlowSkipsContainerStatsWhenDisabled(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "stats" {
			t.Fatal("docker stats must not be called when collect.container_stats is disabled")
		}
		if name == "docker" {
			return []byte("web\trunning\tUp\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: true, method: "socket"}
	c := config.Default()
	if err := c.Set("collect.container_stats", "false"); err != nil {
		t.Fatal(err)
	}
	snap := collectSlow(x, fs, da, c, nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil when disabled", snap.ContainerStats)
	}
}

// TestCollectSlowSkipsContainerStatsWhenDockerUnavailable mirrors the
// existing container-list guard: da.available=false must skip the stats
// call entirely, not just leave the result empty.
func TestCollectSlowSkipsContainerStatsWhenDockerUnavailable(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" {
			t.Fatal("docker must not be called at all when unavailable")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	snap := collectSlow(x, fs, da, config.Default(), nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil when docker unavailable", snap.ContainerStats)
	}
}

// TestCollectSlowSkipsContainerStatsOnNilConfig asserts collectSlow never
// panics when called with a nil *config.Config (defensive: any future
// one-shot caller without a config handy should degrade gracefully rather
// than crash).
func TestCollectSlowSkipsContainerStatsOnNilConfig(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "stats" {
			t.Fatal("docker stats must not be called with a nil config")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: true, method: "socket"}
	snap := collectSlow(x, fs, da, nil, nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil with nil config", snap.ContainerStats)
	}
}

// TestCollectSlowContainerStatsErrorDegradesGracefully asserts a docker
// stats error (daemon busy, container churn mid-call, etc.) leaves
// ContainerStats nil for this tick rather than propagating the error or
// crashing the rest of collectSlow.
func TestCollectSlowContainerStatsErrorDegradesGracefully(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) > 0 && args[0] == "stats" {
			return nil, errors.New("docker daemon busy")
		}
		if name == "docker" {
			return []byte("web\trunning\tUp\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: true, method: "socket"}
	snap := collectSlow(x, fs, da, config.Default(), nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil on stats error", snap.ContainerStats)
	}
	// The rest of the slow collection must still have succeeded.
	if snap.Containers["web"] != "running" {
		t.Errorf("Containers[web] = %q, want running (rest of collectSlow unaffected)", snap.Containers["web"])
	}
}

// TestCollectSlowPopulatesUnits asserts collect.services (default true, and
// unrelated to docker availability/config) fills snap.Units with the full
// systemd unit inventory, alongside (not instead of) the existing
// FailedUnits alerting collection.
func TestCollectSlowPopulatesUnits(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch name {
		case "df":
			return []byte("Filesystem 1B-blocks Used Available Capacity Mounted\n/dev/sda1 100 90 10 90% /\n"), nil
		case "systemctl":
			if len(args) > 0 && args[0] == "--failed" {
				return []byte("nginx.service loaded failed failed A high performance web server\n"), nil
			}
			return []byte("nginx.service    loaded active   running A high performance web server\n" +
				"cron.service     loaded active   running Regular background program\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	snap := collectSlow(x, fs, da, config.Default(), nil, 0)

	if len(snap.Units) != 2 {
		t.Fatalf("Units = %+v, want 2 entries", snap.Units)
	}
	if len(snap.FailedUnits) != 1 || snap.FailedUnits[0] != "nginx.service" {
		t.Errorf("FailedUnits = %+v, want unchanged [nginx.service]", snap.FailedUnits)
	}
}

// TestCollectSlowSkipsUnitsWhenDisabled asserts collect.services=false
// suppresses the `systemctl list-units` call (the fake Exec fails the test
// if it's requested), while the --failed alerting call still runs.
func TestCollectSlowSkipsUnitsWhenDisabled(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "list-units" {
			t.Fatal("systemctl list-units must not be called when collect.services is disabled")
		}
		if name == "systemctl" {
			return []byte("nginx.service loaded failed failed A high performance web server\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	c := config.Default()
	if err := c.Set("collect.services", "false"); err != nil {
		t.Fatal(err)
	}
	snap := collectSlow(x, fs, da, c, nil, 0)
	if snap.Units != nil {
		t.Errorf("Units = %+v, want nil when collect.services disabled", snap.Units)
	}
	if len(snap.FailedUnits) != 1 {
		t.Errorf("FailedUnits = %+v, want the --failed alerting collection unaffected", snap.FailedUnits)
	}
}

// TestCollectSlowSkipsUnitsOnNilConfig mirrors the container-stats nil-config
// guard: collectSlow must not panic (or call list-units) with a nil
// *config.Config.
func TestCollectSlowSkipsUnitsOnNilConfig(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "list-units" {
			t.Fatal("systemctl list-units must not be called with a nil config")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	snap := collectSlow(x, fs, da, nil, nil, 0)
	if snap.Units != nil {
		t.Errorf("Units = %+v, want nil with nil config", snap.Units)
	}
}

func TestContainerMetricSet(t *testing.T) {
	snap := Snapshot{ContainerStats: map[string]ContainerStat{
		"web": {Name: "web", CPUPct: 11.2, MemMiB: 512, NetRxMB: 2.1, NetTxMB: 0.4},
		"db":  {Name: "db", CPUPct: 0.5, MemMiB: 1536, NetRxMB: 0.5, NetTxMB: 0.1},
	}}
	ms := containerMetricSet(snap)
	want := MetricSet{
		"docker:web:cpu": 11.2, "docker:web:mem": 512,
		"docker:db:cpu": 0.5, "docker:db:mem": 1536,
	}
	if len(ms) != len(want) {
		t.Fatalf("containerMetricSet = %+v, want %+v", ms, want)
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("containerMetricSet[%q] = %v, want %v", k, ms[k], v)
		}
	}
}

func TestContainerMetricSetEmpty(t *testing.T) {
	if ms := containerMetricSet(Snapshot{}); len(ms) != 0 {
		t.Fatalf("containerMetricSet(empty) = %+v, want empty", ms)
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
	snap := Snapshot{CPU: 12, MemPct: 34, SwapPct: 5, Load1: 1.5, Load5: 1.2, Load15: 0.9, TempC: 60}
	ms := fastMetricSet(snap)
	want := MetricSet{"cpu": 12, "mem": 34, "swap": 5, "load1": 1.5, "load5": 1.2, "load15": 0.9, "temp": 60}
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
	snap := Snapshot{CPU: 12, MemPct: 34, SwapPct: 5, Load1: 1.5, Load5: 1.2, Load15: 0.9, TempC: 0}
	ms := fastMetricSet(snap)
	if _, ok := ms["temp"]; ok {
		t.Fatalf("fastMetricSet with TempC=0 = %+v, want no \"temp\" key", ms)
	}
	if len(ms) != 6 {
		t.Fatalf("fastMetricSet = %+v, want exactly cpu/mem/swap/load1/load5/load15", ms)
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

func TestNetRateMetricSet(t *testing.T) {
	snap := Snapshot{NetRates: map[string]IfaceRate{
		"eth0": {RxBps: 1234.5, TxBps: 67.8},
	}}
	ms := netRateMetricSet(snap.NetRates)
	want := MetricSet{"net:eth0:rx": 1234.5, "net:eth0:tx": 67.8}
	if len(ms) != len(want) {
		t.Fatalf("netRateMetricSet = %+v, want %+v", ms, want)
	}
	for k, v := range want {
		if ms[k] != v {
			t.Errorf("netRateMetricSet[%q] = %v, want %v", k, ms[k], v)
		}
	}
}

func TestNetRateMetricSetEmpty(t *testing.T) {
	if ms := netRateMetricSet(nil); len(ms) != 0 {
		t.Fatalf("netRateMetricSet(nil) = %+v, want empty", ms)
	}
}

// TestDigestNowFromStore seeds a memory SampleStore with cpu/mem points and a
// downtime event inside the digest window, and asserts digestNow (reading
// exclusively via store.Query/store.Events, no legacy Store involved)
// reports the right peaks, sample count, and downtime summary.
func TestDigestNowFromStore(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := newMemStore(StoreOptions{})

	within := now.Add(-2 * time.Hour).Unix()
	if err := store.Append(within, MetricSet{"cpu": 30, "mem": 40}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(now.Add(-1*time.Hour).Unix(), MetricSet{"cpu": 77, "mem": 61}); err != nil {
		t.Fatal(err)
	}
	// Outside the 1-day window: must not affect peaks or sample count.
	if err := store.Append(now.AddDate(0, 0, -5).Unix(), MetricSet{"cpu": 99, "mem": 99}); err != nil {
		t.Fatal(err)
	}

	end := now.Add(-3 * time.Hour)
	start := end.Add(-10 * time.Minute)
	if err := store.AppendEvent(DownEvent{
		Type:        "net_down",
		Start:       start.Unix(),
		End:         end.Unix(),
		DurationSec: int64(end.Sub(start) / time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	s := digestNow(store, now, 1, "📊 daily digest", 48*time.Hour)
	if !strings.Contains(s, "peak CPU: 77%") {
		t.Errorf("digest = %q, want peak CPU 77%%", s)
	}
	if !strings.Contains(s, "peak mem: 61%") {
		t.Errorf("digest = %q, want peak mem 61%%", s)
	}
	if !strings.Contains(s, "samples: 2") {
		t.Errorf("digest = %q, want samples: 2 (points inside window only)", s)
	}
	if !strings.Contains(s, "across 1 events") {
		t.Errorf("digest = %q, want downtime summary across 1 events", s)
	}
}

// TestDigestNowRes1m covers a weekly-style window (days=7) whose start is
// older than raw retention, so PickResolution selects Res1m for peaks. The
// sample count is always taken at Res1m regardless, so it stays cadence-stable
// (~per-minute) rather than inflating to the ~5s raw cadence. memStore ignores
// res and serves one series, so this exercises the code path and asserts the
// count reflects the points inside the window.
func TestDigestNowRes1m(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store := newMemStore(StoreOptions{})

	// Three points spread across the 7-day window, all older than a 48h raw
	// retention so the peak query resolves to Res1m.
	for i, off := range []time.Duration{6 * 24 * time.Hour, 5 * 24 * time.Hour, 4 * 24 * time.Hour} {
		ts := now.Add(-off).Unix()
		cpu := float64(10 + i*20) // 10, 30, 50
		if err := store.Append(ts, MetricSet{"cpu": cpu, "mem": cpu + 5}); err != nil {
			t.Fatal(err)
		}
	}

	s := digestNow(store, now, 7, "📆 weekly rollup", 48*time.Hour)
	if !strings.Contains(s, "peak CPU: 50%") {
		t.Errorf("digest = %q, want peak CPU 50%%", s)
	}
	if !strings.Contains(s, "peak mem: 55%") {
		t.Errorf("digest = %q, want peak mem 55%%", s)
	}
	if !strings.Contains(s, "samples: 3") {
		t.Errorf("digest = %q, want samples: 3 (1m-based count)", s)
	}
	if !strings.Contains(s, "(7d)") {
		t.Errorf("digest = %q, want 7d window label", s)
	}
}

// TestDigestNowNilStore verifies digestNow degrades to an empty digest
// (rather than panicking) when the SampleStore failed to open at startup.
func TestDigestNowNilStore(t *testing.T) {
	s := digestNow(nil, time.Now(), 1, "📊 daily digest", 48*time.Hour)
	if !strings.Contains(s, "samples: 0") {
		t.Errorf("nil-store digest = %q, want samples: 0", s)
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
