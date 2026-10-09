package trinetra

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
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

// TestBuildSlowChecksBinaryWording confirms docker/service/smart checks carry human
// FireMsg/RecoverMsg wording (issue #5) while numeric checks.
func TestBuildSlowChecksBinaryWording(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		Disks:       map[string]float64{"/": 85},
		Containers:  map[string]string{"web": "exited"},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sdb": "FAILED"},
	}
	active := map[string]ActiveAlert{}
	checks := buildSlowChecks(snap, c, active)

	byKey := map[string]Check{}
	for _, ch := range checks {
		byKey[ch.Key] = ch
	}

	docker, ok := byKey["docker:web"]
	if !ok {
		t.Fatal("missing docker:web check")
	}
	if docker.FireMsg != "container web is down (exited)" {
		t.Errorf("docker:web FireMsg = %q, want %q", docker.FireMsg, "container web is down (exited)")
	}
	if docker.RecoverMsg != "container web recovered" {
		t.Errorf("docker:web RecoverMsg = %q, want %q", docker.RecoverMsg, "container web recovered")
	}

	service, ok := byKey["service:nginx.service"]
	if !ok {
		t.Fatal("missing service:nginx.service check")
	}
	if service.FireMsg != "unit nginx.service failed" {
		t.Errorf("service FireMsg = %q, want %q", service.FireMsg, "unit nginx.service failed")
	}
	if service.RecoverMsg != "unit nginx.service recovered" {
		t.Errorf("service RecoverMsg = %q, want %q", service.RecoverMsg, "unit nginx.service recovered")
	}

	smart, ok := byKey["smart:/dev/sdb"]
	if !ok {
		t.Fatal("missing smart:/dev/sdb check")
	}
	if smart.FireMsg != "SMART FAILED on /dev/sdb" {
		t.Errorf("smart FireMsg = %q, want %q", smart.FireMsg, "SMART FAILED on /dev/sdb")
	}
	if smart.RecoverMsg != "SMART health recovered on /dev/sdb" {
		t.Errorf("smart RecoverMsg = %q, want %q", smart.RecoverMsg, "SMART health recovered on /dev/sdb")
	}

	disk, ok := byKey["disk:/"]
	if !ok {
		t.Fatal("missing disk:/ check")
	}
	if disk.FireMsg != "" || disk.RecoverMsg != "" {
		t.Errorf("disk:/ must stay numeric, got FireMsg=%q RecoverMsg=%q", disk.FireMsg, disk.RecoverMsg)
	}
}

// TestBuildSlowChecksRecoverySweepWording confirms the value=0 recovery-sweep
// sites (for a service no longer failed, or a docker/smart target that
// disappeared from the snapshot) also carry the human RecoverMsg, so a
// recovery emitted from any of these sites still reads humanely.
func TestBuildSlowChecksRecoverySweepWording(t *testing.T) {
	c := config.Default()
	snap := Snapshot{
		Containers:  map[string]string{},
		SmartHealth: map[string]string{},
	}
	active := map[string]ActiveAlert{
		"service:cron.service": {Since: 1},
		"docker:old-web":       {Since: 1},
		"smart:/dev/sdz":       {Since: 1},
	}
	checks := buildSlowChecks(snap, c, active)
	byKey := map[string]Check{}
	for _, ch := range checks {
		byKey[ch.Key] = ch
	}

	if got := byKey["service:cron.service"]; got.RecoverMsg != "unit cron.service recovered" {
		t.Errorf("service sweep RecoverMsg = %q, want %q", got.RecoverMsg, "unit cron.service recovered")
	}
	if got := byKey["docker:old-web"]; got.RecoverMsg != "container old-web recovered" {
		t.Errorf("docker sweep RecoverMsg = %q, want %q", got.RecoverMsg, "container old-web recovered")
	}
	if got := byKey["smart:/dev/sdz"]; got.RecoverMsg != "SMART health recovered on /dev/sdz" {
		t.Errorf("smart sweep RecoverMsg = %q, want %q", got.RecoverMsg, "SMART health recovered on /dev/sdz")
	}
}

func keysOf(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestFastSlowUnionMatchesBuildChecks guards against a check silently dropping out of both
// tiers during future edits.
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

// TestEventToAlertEscapesHTML is the Critical regression: anomaly alerts are dispatched via
// SendMessage (now parse_mode=HTML).
func TestEventToAlertEscapesHTML(t *testing.T) {
	a := eventToAlert(Event{Key: "docker:web", Kind: "fire", Text: "container <b>x</b> is down (&exited)", Critical: true}, 0)
	if strings.Contains(a.Title, "<b>") || strings.Contains(a.Title, "</b>") {
		t.Fatalf("Title not escaped: %q", a.Title)
	}
	if !strings.Contains(a.Title, "&lt;b&gt;") || !strings.Contains(a.Title, "&amp;exited") {
		t.Fatalf("Title should be HTML-escaped, got %q", a.Title)
	}
	// The rendered message (what actually reaches SendMessage) must carry no
	// stray raw angle brackets from the dynamic content.
	msg := formatAlert(a)
	if strings.Contains(msg, "<b>") || strings.Contains(msg, "</b>") {
		t.Fatalf("formatAlert leaked raw tags: %q", msg)
	}
}

// TestBootReportKeepsIntentionalHTML proves the escaping in eventToAlert did NOT over-reach
// into the boot-report path.
func TestBootReportKeepsIntentionalHTML(t *testing.T) {
	snap := Snapshot{CPU: 10, Online: true, Disks: map[string]float64{"/": 30}}
	report := formatBootReport([]DownEvent{{Type: "power_down", Start: 0, End: 60, DurationSec: 60}}, renderStatus(snap, config.Default()))
	if !strings.Contains(report, "<pre>") || !strings.Contains(report, "</pre>") {
		t.Fatalf("boot report lost its intentional <pre> markup: %q", report)
	}
	if strings.Contains(report, "&lt;pre&gt;") {
		t.Fatalf("boot report's HTML was wrongly escaped: %q", report)
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
		switch {
		case name == "df" && len(args) > 0 && args[0] == "-PT":
			return []byte("Filesystem Type 1-blocks Used Available Capacity Mounted on\n/dev/sda1 ext4 100 90 10 90% /\n"), nil
		case name == "df" && len(args) > 0 && args[0] == "-Pi":
			return []byte("Filesystem Inodes IUsed IFree IUse% Mounted on\n/dev/sda1 1000 100 900 10% /\n"), nil
		case name == "docker":
			if len(args) > 0 && args[0] == "stats" {
				return []byte("web\t3.00%\t100MiB / 1GiB\t1MB / 1MB\n"), nil
			}
			return []byte("web\trunning\tUp 3 hours\n"), nil
		case name == "systemctl":
			return []byte("nginx.service loaded failed failed A high performance web server\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "--scan":
			return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
		case name == "smartctl":
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: true, method: "socket"}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)

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

// TestCollectSlowPopulatesDiskDetailAndSmartAttrs asserts collectSlow merges `df -PT -B1`
// with `df -Pi` into snap.DiskDetail keyed by mount.
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

	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)

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

// TestCollectSlowFiltersDockerOverlayAndPseudoMounts is the regression test
// for a root daemon on a docker host seeing dozens of `overlay` mounts plus
// squashfs/tmpfs/nsfs pseudo-mounts in `df -PT -B1`: only the real ext4
// mounts may survive in snap.Disks and snap.DiskDetail.
func TestCollectSlowFiltersDockerOverlayAndPseudoMounts(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "df" && len(args) > 0 && args[0] == "-PT":
			return []byte("Filesystem     Type     1-blocks   Used   Available Capacity Mounted on\n" +
				"/dev/sda1      ext4     100        60     40        60% /\n" +
				"/dev/sda2      ext4     200        20     180       10% /boot\n" +
				"/dev/sdb1      ext4     500        100    400       20% /mnt/data\n" +
				"overlay        overlay  999999     999999 0         100% /var/lib/docker/overlay2/abc123/merged\n" +
				"/dev/loop0     squashfs 12345      12345  0         100% /snap/core/1234\n" +
				"tmpfs          tmpfs    1000       0      1000      0% /dev/shm\n"), nil
		case name == "df" && len(args) > 0 && args[0] == "-Pi":
			return nil, errNotExist
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}

	origDial := connDial
	connDial = func(host string) bool { return true }
	defer func() { connDial = origDial }()

	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)

	wantDisks := map[string]float64{"/": 60, "/boot": 10, "/mnt/data": 20}
	if len(snap.Disks) != len(wantDisks) {
		t.Fatalf("Disks = %+v, want exactly %+v", snap.Disks, wantDisks)
	}
	for m, pct := range wantDisks {
		if snap.Disks[m] != pct {
			t.Errorf("Disks[%s] = %v, want %v", m, snap.Disks[m], pct)
		}
	}
	for _, junk := range []string{"/var/lib/docker/overlay2/abc123/merged", "/snap/core/1234", "/dev/shm"} {
		if _, ok := snap.Disks[junk]; ok {
			t.Errorf("Disks contains junk mount %q, want filtered out", junk)
		}
		if _, ok := snap.DiskDetail[junk]; ok {
			t.Errorf("DiskDetail contains junk mount %q, want filtered out", junk)
		}
	}
	if len(snap.DiskDetail) != len(wantDisks) {
		t.Fatalf("DiskDetail = %+v, want exactly the same %d real mounts as Disks", snap.DiskDetail, len(wantDisks))
	}
	if dd := snap.DiskDetail["/mnt/data"]; dd.FsType != "ext4" || dd.Device != "/dev/sdb1" {
		t.Errorf("DiskDetail[/mnt/data] = %+v, want fstype ext4 device /dev/sdb1", dd)
	}
}

// TestCollectSlowSkipsSmartAttrsWhenDisabled asserts collect.smart_attrs=false suppresses
// the `smartctl -A <dev>` call.
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
	snap := collectSlow(x, fs, da, c, nil, 0, nil, 0)
	if snap.SmartAttrs != nil {
		t.Errorf("SmartAttrs = %+v, want nil when collect.smart_attrs disabled", snap.SmartAttrs)
	}
	if snap.SmartHealth["/dev/sda"] != "PASSED" {
		t.Errorf("SmartHealth[/dev/sda] = %q, want PASSED (health check unaffected)", snap.SmartHealth["/dev/sda"])
	}
}

// TestCollectSlowSkipsSmartAttrsOnNilConfig mirrors the container-stats/units nil-config
// guard: collectSlow must not panic (or call `smartctl -A`) with a nil *config.Config.
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
	snap := collectSlow(x, fs, da, nil, nil, 0, nil, 0)
	if snap.SmartAttrs != nil {
		t.Errorf("SmartAttrs = %+v, want nil with nil config", snap.SmartAttrs)
	}
}

// smartScanFakeExec returns a fakeExec that serves canned smartctl/df output and counts
// every `smartctl --scan` invocation via *scans.
func smartScanFakeExec(scans *int) fakeExec {
	return fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		switch {
		case name == "df":
			return []byte("Filesystem 1B-blocks Used Available Capacity Mounted\n/dev/sda1 100 90 10 90% /\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "--scan":
			*scans++
			return []byte("/dev/sda -d sat # /dev/sda [SAT], ATA device\n"), nil
		case name == "smartctl" && len(args) > 0 && args[0] == "-H":
			return []byte("SMART overall-health self-assessment test result: PASSED\n"), nil
		}
		return nil, errNotExist
	}}
}

// TestCollectSlowSmartThrottleReusesCache asserts that a second collectSlow call sharing
// one *smartCache within smartIntervalSec of the first does NOT re-invoke smartctl.
func TestCollectSlowSmartThrottleReusesCache(t *testing.T) {
	var scans int
	x := smartScanFakeExec(&scans)
	fs := fakeFS{}
	da := dockerAccess{available: false}
	var sc smartCache

	snap1 := collectSlow(x, fs, da, config.Default(), nil, 1000, &sc, 1800)
	if scans != 1 {
		t.Fatalf("scans after first call = %d, want 1", scans)
	}
	if snap1.SmartHealth["/dev/sda"] != "PASSED" {
		t.Fatalf("first call SmartHealth[/dev/sda] = %q, want PASSED", snap1.SmartHealth["/dev/sda"])
	}

	snap2 := collectSlow(x, fs, da, config.Default(), nil, 1005, &sc, 1800)
	if scans != 1 {
		t.Errorf("scans after second call (5s later, interval 1800s) = %d, want still 1 (cached)", scans)
	}
	if snap2.SmartHealth["/dev/sda"] != "PASSED" {
		t.Errorf("second call SmartHealth[/dev/sda] = %q, want PASSED (from cache)", snap2.SmartHealth["/dev/sda"])
	}
}

// TestCollectSlowSmartRescansAfterInterval asserts that once nowUnix has advanced past
// smartIntervalSec since the last real scan.
func TestCollectSlowSmartRescansAfterInterval(t *testing.T) {
	var scans int
	x := smartScanFakeExec(&scans)
	fs := fakeFS{}
	da := dockerAccess{available: false}
	var sc smartCache

	collectSlow(x, fs, da, config.Default(), nil, 1000, &sc, 1800)
	if scans != 1 {
		t.Fatalf("scans after first call = %d, want 1", scans)
	}

	collectSlow(x, fs, da, config.Default(), nil, 1000+1800, &sc, 1800)
	if scans != 2 {
		t.Errorf("scans after second call (interval elapsed) = %d, want 2 (re-scanned)", scans)
	}
}

// TestCollectSlowSmartNilCacheAlwaysScans asserts the one-shot path (nil
// *smartCache, e.g. collectSnapshot) never throttles: every call scans.
func TestCollectSlowSmartNilCacheAlwaysScans(t *testing.T) {
	var scans int
	x := smartScanFakeExec(&scans)
	fs := fakeFS{}
	da := dockerAccess{available: false}

	collectSlow(x, fs, da, config.Default(), nil, 1000, nil, 0)
	collectSlow(x, fs, da, config.Default(), nil, 1005, nil, 0)
	if scans != 2 {
		t.Errorf("scans with nil cache across two calls = %d, want 2 (always scan)", scans)
	}
}

// TestCollectSlowDiskDetailProjectsDaysToFull asserts that when a store IS supplied.
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

	snap := collectSlow(x, fs, da, config.Default(), store, nowUnix, nil, 0)

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

// TestSmartMetricSet asserts smartMetricSet emits one "smart:<dev>:temp" entry per device
// with a known (>0) temperature, and omits devices whose attribute set didn't report one.
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

// TestCollectSlowSkipsContainerStatsWhenDisabled asserts the collect.container_stats=false
// opt-out actually suppresses the `docker stats` call.
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
	snap := collectSlow(x, fs, da, c, nil, 0, nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil when disabled", snap.ContainerStats)
	}
}

// TestCollectSlowSkipsContainerStatsWhenDockerUnavailable mirrors the existing
// container-list guard: da.available=false must skip the stats call entirely.
func TestCollectSlowSkipsContainerStatsWhenDockerUnavailable(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "docker" {
			t.Fatal("docker must not be called at all when unavailable")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)
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
	snap := collectSlow(x, fs, da, nil, nil, 0, nil, 0)
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
	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)
	if snap.ContainerStats != nil {
		t.Errorf("ContainerStats = %+v, want nil on stats error", snap.ContainerStats)
	}
	// The rest of the slow collection must still have succeeded.
	if snap.Containers["web"] != "running" {
		t.Errorf("Containers[web] = %q, want running (rest of collectSlow unaffected)", snap.Containers["web"])
	}
}

// TestCollectSlowPopulatesUnits asserts collect.services (default true, and unrelated to
// docker availability/config) fills snap.Units with the full systemd unit inventory.
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

	snap := collectSlow(x, fs, da, config.Default(), nil, 0, nil, 0)

	if len(snap.Units) != 2 {
		t.Fatalf("Units = %+v, want 2 entries", snap.Units)
	}
	if len(snap.FailedUnits) != 1 || snap.FailedUnits[0] != "nginx.service" {
		t.Errorf("FailedUnits = %+v, want unchanged [nginx.service]", snap.FailedUnits)
	}
}

// TestCollectSlowSkipsUnitsWhenDisabled asserts collect.services=false suppresses the
// `systemctl list-units` call (the fake Exec fails the test if it's requested).
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
	snap := collectSlow(x, fs, da, c, nil, 0, nil, 0)
	if snap.Units != nil {
		t.Errorf("Units = %+v, want nil when collect.services disabled", snap.Units)
	}
	if len(snap.FailedUnits) != 1 {
		t.Errorf("FailedUnits = %+v, want the --failed alerting collection unaffected", snap.FailedUnits)
	}
}

// TestCollectSlowSkipsUnitsOnNilConfig mirrors the container-stats nil-config guard:
// collectSlow must not panic (or call list-units) with a nil *config.Config.
func TestCollectSlowSkipsUnitsOnNilConfig(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 0 && args[0] == "list-units" {
			t.Fatal("systemctl list-units must not be called with a nil config")
		}
		return nil, errNotExist
	}}
	fs := fakeFS{}
	da := dockerAccess{available: false}
	snap := collectSlow(x, fs, da, nil, nil, 0, nil, 0)
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

// TestEventToAlertDispatchRespectsQuietHours exercises eventToAlert feeding straight into a
// Dispatcher built from fake, in-memory Channels (rather than going through config).
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

	// During quiet hours: only the critical-overrides-quiet channel should receive the
	// critical alert; the warning must reach neither channel.
	d.Dispatch(critical, true)
	d.Dispatch(warning, true)

	if got := len(always.received()); got != 1 {
		t.Errorf("critical-overrides-quiet channel received %d alerts during quiet hours, want 1", got)
	}
	if got := len(quietAware.received()); got != 0 {
		t.Errorf("quiet-respecting channel received %d alerts during quiet hours, want 0", got)
	}

	// Outside quiet hours the warning should now reach the quiet-respecting channel too.
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

// TestDigestNowFromStore seeds a memory SampleStore with cpu/mem points and a downtime
// event inside the digest window, and asserts digestNow.
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

// TestDigestNowRes1m covers a weekly-style window (days=7) whose start is older than raw
// retention, so PickResolution selects Res1m for peaks.
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
	// Integration-style: exercise the actual write path a fast/slow tick takes --
	// fastMetricSet/slowMetricSet feeding SampleStore.Append -- against a real.
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

func TestSlowHubVersioning(t *testing.T) {
	h := &slowHub{}
	if _, _, ok := h.latest(); ok {
		t.Fatal("empty hub should report no snapshot")
	}
	h.publish(Snapshot{Online: true})
	s, v1, ok := h.latest()
	if !ok || !s.Online {
		t.Fatal("expected published snapshot")
	}
	h.publish(Snapshot{Online: false})
	_, v2, _ := h.latest()
	if v2 <= v1 {
		t.Fatalf("version did not advance: %d -> %d", v1, v2)
	}
}

// TestReloadOnHUPRestoresManagedValueAfterExternalEdit: a SIGHUP after an external edit to
// config.json.
func TestReloadOnHUPRestoresManagedValueAfterExternalEdit(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// What's on disk right now: an external edit diverged the managed key
	// back to 50, while the committed value (below) is 85.
	diverged := config.Default()
	diverged.Thresholds.CPUPct = 50
	if err := diverged.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	sidecarPath := filepath.Join(dir, "managed.json")
	sidecar := managedChildFileV1{Version: 1, Values: map[string]string{"thresholds.cpu_pct": "85"}}
	b, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	mc := loadManagedChild(sidecarPath, nil, nil, nil)

	var persisted *config.Config
	reload := func(c *config.Config) error {
		reimposeManagedValues(mc, c)
		persisted = c
		return nil
	}

	msg, err := reloadOnHUP(cfgPath, reload)
	if err != nil {
		t.Fatalf("reloadOnHUP err = %v", err)
	}
	if msg != "config reloaded" {
		t.Fatalf("msg = %q, want %q (unchanged observable behaviour)", msg, "config reloaded")
	}
	if persisted == nil {
		t.Fatal("reload was never called")
	}
	if got, _ := persisted.Get("thresholds.cpu_pct"); got != "85" {
		t.Fatalf("reloaded thresholds.cpu_pct = %q, want the managed value 85 restored, not the diverged 50 adopted", got)
	}
}

// TestReloadOnHUPSilentOnLoadFailure pins that a cfgPath that fails to load is
// silently ignored (no message, no error).
func TestReloadOnHUPSilentOnLoadFailure(t *testing.T) {
	// A MISSING cfgPath is not a Load failure (config.Load treats it as "use
	// defaults"); a genuine failure needs a file that exists but fails to parse.
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	reload := func(*config.Config) error { called = true; return nil }
	msg, err := reloadOnHUP(cfgPath, reload)
	if err != nil || msg != "" {
		t.Fatalf("msg=%q err=%v, want empty/nil on a load failure", msg, err)
	}
	if called {
		t.Fatal("reload must not be called when Load fails")
	}
}

// TestReloadOnHUPSurfacesReloadError: a reload failure (e.g. saveDaemonCfg's
// write erroring) is returned for the caller to report.
func TestReloadOnHUPSurfacesReloadError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := config.Default().Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("boom")
	reload := func(*config.Config) error { return wantErr }
	msg, err := reloadOnHUP(cfgPath, reload)
	if err != wantErr || msg != "" {
		t.Fatalf("msg=%q err=%v, want (\"\", %v)", msg, err, wantErr)
	}
}
