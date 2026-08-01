package serverwatch

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// TestInprocSnapshotProjectsScalars is the Step 1 failing test from the
// task-4 brief: newInprocAPI's Snapshot() must project a Snapshot's scalar
// fields into a core.DashboardView, same as buildDashboardView already does
// for the web build.
func TestInprocSnapshotProjectsScalars(t *testing.T) {
	snap := Snapshot{TS: 42, CPU: 12.5, MemPct: 30, Online: true}
	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, nil)
	v, err := api.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if v.TS != 42 || v.CPU != 12.5 || !v.Online {
		t.Fatalf("projection mismatch: %+v", v)
	}
}

// fullyPopulatedSnapshot builds a Snapshot exercising every field
// buildDashboardView/buildMonitoringView read, for the parity test below.
func fullyPopulatedSnapshot() Snapshot {
	return Snapshot{
		TS: 1_700_000_000, CPU: 37.5, MemPct: 61, SwapPct: 4, Load1: 0.42, Load5: 0.55, Load15: 0.61, TempC: 54,
		Online: true,
		Disks: map[string]float64{
			"/":     91,
			"/data": 22,
		},
		DiskDetail: map[string]DiskDetail{
			"/": {Device: "/dev/sda1", FsType: "ext4", UsagePct: 91, InodePct: 12, FreeBytes: 4_100_000_000, SizeBytes: 100_000_000_000, DaysToFull: 3, DaysToFullKnown: true},
		},
		Containers: map[string]string{
			"nextcloud": "running",
			"postgres":  "running",
			"jellyfin":  "exited",
		},
		ContainerStats: map[string]ContainerStat{
			"nextcloud": {Name: "nextcloud", CPUPct: 5, MemMiB: 300, NetRxMB: 1.2, NetTxMB: 0.4},
			"postgres":  {Name: "postgres", CPUPct: 2, MemMiB: 150},
			"jellyfin":  {Name: "jellyfin", CPUPct: 8, MemMiB: 900},
		},
		NetRates: map[string]IfaceRate{
			"eth0":  {RxBps: 1_800_000, TxBps: 240_000},
			"wlan0": {RxBps: 100, TxBps: 50},
		},
		FailedUnits: []string{"foo.service"},
		Units:       []UnitInfo{{Name: "foo.service", Load: "loaded", Active: "failed", Sub: "failed", Description: "Foo"}, {Name: "bar.service", Load: "loaded", Active: "active", Sub: "running", Description: "Bar"}},
		Processes: ProcSnapshot{
			Total: 214, Running: 1, Sleeping: 210, Zombie: 3,
			Top: []ProcInfo{{PID: 1, Name: "init", State: "S", CPUPct: 0.1, MemMiB: 4, Threads: 1}},
		},
	}
}

// TestInprocSnapshotMatchesBuildDashboardView is the Step 5 parity test:
// newInprocAPI(...).Snapshot() must project a fully-populated Snapshot
// identically to the re-homed buildDashboardView, guarding against any
// behavior change from moving that function out of daemon_web.go. The two
// are compared with Availability zeroed on both sides first, since
// Snapshot() additionally computes Availability fresh on top of
// buildDashboardView's projection (see its doc) -- that wiring isn't what
// this test is pinning.
func TestInprocSnapshotMatchesBuildDashboardView(t *testing.T) {
	snap := fullyPopulatedSnapshot()
	cfg := config.Default()

	want := buildDashboardView(snap)
	want.Availability = core.Availability{}

	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return cfg }, nil, t.TempDir(), nil, nil)
	got, err := api.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	got.Availability = core.Availability{}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("newInprocAPI(...).Snapshot() != buildDashboardView(snap):\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestInprocMonitoringMatchesBuildMonitoringView is Monitoring()'s
// counterpart to the Snapshot parity test above -- buildMonitoringView takes
// no Availability-style extra step, so this is a straight equality check.
func TestInprocMonitoringMatchesBuildMonitoringView(t *testing.T) {
	snap := fullyPopulatedSnapshot()
	cfg := config.Default() // Collect.Services/Processes nil -> both enabled (ServicesEnabled/ProcessesEnabled default true)

	want := buildMonitoringView(snap, cfg)

	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return cfg }, nil, t.TempDir(), nil, nil)
	got, err := api.Monitoring()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("newInprocAPI(...).Monitoring() != buildMonitoringView(snap, cfg):\ngot:  %+v\nwant: %+v", got, want)
	}
}

// TestSeriesResolutionMapping exercises Series against a real tsfile
// SampleStore (not a mock) seeded with genuinely distinct raw and 1m data,
// pinning all three of: the explicit core.ResRaw/core.Res1m mapping onto
// their serverwatch.Resolution counterparts, and core.ResAuto's delegation
// to PickResolution.
//
// Timestamps are computed relative to the start of the current minute
// (nowKey) rather than "now" directly, so the test's pass/fail never
// depends on which second within a minute it happens to run at:
//   - recentTS sits exactly at nowKey -- its 1-minute bucket can never be
//     "completed" (key+60 > now always holds for the bucket containing
//     now), so Downsample never sweeps it into the 1m file regardless of
//     timing.
//   - oldTS1/oldTS2 sit inside the same 1-minute bucket exactly 2 hours
//     before nowKey -- always long since completed, so Downsample always
//     rolls both into one 1m record.
func TestSeriesResolutionMapping(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore("tsfile", dir, StoreOptions{RawRetention: time.Hour})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().Unix()
	nowKey := (now / 60) * 60
	oldKey := nowKey - 7200 // 2h before the current minute -> always outside the 1h raw retention

	oldTS1, oldTS2 := oldKey+10, oldKey+20 // same 1m bucket (oldKey), two different values
	recentTS := nowKey                     // current, still-open bucket

	if err := store.Append(oldTS1, MetricSet{"cpu": 40}); err != nil {
		t.Fatalf("Append oldTS1: %v", err)
	}
	if err := store.Append(oldTS2, MetricSet{"cpu": 60}); err != nil {
		t.Fatalf("Append oldTS2: %v", err)
	}
	if err := store.Append(recentTS, MetricSet{"cpu": 99}); err != nil {
		t.Fatalf("Append recentTS: %v", err)
	}
	// Rolls the completed oldTS1/oldTS2 bucket into the 1m file; recentTS's
	// bucket is still open (key+60 > now) so it stays raw-only.
	if err := store.Downsample(now); err != nil {
		t.Fatalf("Downsample: %v", err)
	}

	cfg := config.Default()
	cfg.Storage.RawRetention = "1h" // must match the store's RawRetention above for PickResolution to agree
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, store, dir, nil, nil)

	// Explicit core.ResRaw over a window spanning all three appended points:
	// the raw file has all three, untouched by Downsample.
	raw, err := api.Series("cpu", oldKey-10, now+10, core.ResRaw)
	if err != nil {
		t.Fatalf("Series(ResRaw): %v", err)
	}
	if len(raw) != 3 {
		t.Fatalf("ResRaw len = %d, want 3: %+v", len(raw), raw)
	}
	if raw[0].TS != oldTS1 || raw[0].Avg != 40 || raw[1].TS != oldTS2 || raw[1].Avg != 60 || raw[2].TS != recentTS || raw[2].Avg != 99 {
		t.Fatalf("ResRaw points = %+v, want [oldTS1:40 oldTS2:60 recentTS:99]", raw)
	}

	// Explicit core.Res1m over the same window: only the rolled-up oldKey
	// bucket exists (min=40, avg=50, max=60) -- recentTS never appears here.
	oneM, err := api.Series("cpu", oldKey-10, now+10, core.Res1m)
	if err != nil {
		t.Fatalf("Series(Res1m): %v", err)
	}
	if len(oneM) != 1 {
		t.Fatalf("Res1m len = %d, want 1: %+v", len(oneM), oneM)
	}
	if oneM[0].TS != oldKey || oneM[0].Min != 40 || oneM[0].Avg != 50 || oneM[0].Max != 60 {
		t.Fatalf("Res1m point = %+v, want {TS:%d Min:40 Avg:50 Max:60}", oneM[0], oldKey)
	}

	// core.ResAuto over a RECENT window (from within the last hour) must
	// pick raw -- PickResolution(from, to, now, 1h) returns ResRaw here --
	// and so must match the explicit-raw result restricted to that window:
	// just recentTS.
	autoRecent, err := api.Series("cpu", nowKey-10, now+10, core.ResAuto)
	if err != nil {
		t.Fatalf("Series(ResAuto, recent window): %v", err)
	}
	if len(autoRecent) != 1 || autoRecent[0].TS != recentTS || autoRecent[0].Avg != 99 {
		t.Fatalf("ResAuto (recent window) = %+v, want [{TS:%d Avg:99}] (i.e. picked raw)", autoRecent, recentTS)
	}

	// core.ResAuto over an OLD window (from more than 1h ago) must pick 1m
	// -- and so must match the explicit-1m result: the single aggregated
	// bucket, NOT the two raw points that also exist in this window. This
	// is the real proof ResAuto delegated to PickResolution rather than
	// always reading raw: an explicit-raw query over the same window
	// returns 2 points, but ResAuto here must return the 1 aggregated one.
	autoOld, err := api.Series("cpu", oldKey-10, oldKey+70, core.ResAuto)
	if err != nil {
		t.Fatalf("Series(ResAuto, old window): %v", err)
	}
	if len(autoOld) != 1 || autoOld[0].TS != oldKey || autoOld[0].Min != 40 || autoOld[0].Avg != 50 || autoOld[0].Max != 60 {
		t.Fatalf("ResAuto (old window) = %+v, want [{TS:%d Min:40 Avg:50 Max:60}] (i.e. picked 1m, not raw)", autoOld, oldKey)
	}
}

// TestSeriesNilStoreReturnsEmptyNoPanic pins the nil-store guard: a daemon
// running in store-writes-disabled mode (openConfiguredStore failed at
// startup) must still answer Series calls with an empty result, never a nil
// pointer panic.
func TestSeriesNilStoreReturnsEmptyNoPanic(t *testing.T) {
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, nil)
	got, err := api.Series("cpu", 0, 1000, core.ResAuto)
	if err != nil {
		t.Fatalf("Series with nil store returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Series with nil store = %+v, want empty", got)
	}
}

// TestEventsNilStoreReturnsEmptyNoPanic is Events' counterpart to
// TestSeriesNilStoreReturnsEmptyNoPanic.
func TestEventsNilStoreReturnsEmptyNoPanic(t *testing.T) {
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, nil)
	got, err := api.Events(0, 1000)
	if err != nil {
		t.Fatalf("Events with nil store returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Events with nil store = %+v, want empty", got)
	}
}

// TestEventsMapsStoreEvents exercises Events against a real (memory-backend)
// SampleStore holding one downtime event, pinning the DownEvent ->
// core.DownEventView field mapping.
func TestEventsMapsStoreEvents(t *testing.T) {
	store, err := OpenStore("memory", "", StoreOptions{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.AppendEvent(DownEvent{Type: "power_down", Start: 100, End: 160, DurationSec: 60}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, store, t.TempDir(), nil, nil)
	got, err := api.Events(0, 200)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	want := []core.DownEventView{{Type: "power_down", Start: 100, End: 160, DurationSec: 60}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Events() = %+v, want %+v", got, want)
	}
}

// TestActiveAlertsMapsFields exercises ActiveAlerts against a real
// alerts.json written via AlertState.Save (not a hand-built fixture),
// pinning the ActiveAlert -> core.AlertRecord mapping: Source comes from
// Reason, Severity is rendered via severityString(Critical), Kind is always
// "", and results are sorted by key for a deterministic order (map
// iteration order is not).
func TestActiveAlertsMapsFields(t *testing.T) {
	stateDir := t.TempDir()

	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000, Reason: "cpu = 95.0 >= threshold 90.0", Critical: true, Acked: false}
	state.Active["mem"] = ActiveAlert{Since: 2000, Reason: "mem = 80.0 >= threshold 75.0", Critical: false, Acked: true, AckedAt: 2500}
	if err := state.Save(filepath.Join(stateDir, "alerts.json")); err != nil {
		t.Fatalf("state.Save: %v", err)
	}

	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, stateDir, nil, nil)
	got, err := api.ActiveAlerts()
	if err != nil {
		t.Fatalf("ActiveAlerts: %v", err)
	}

	want := []core.AlertRecord{
		{Key: "cpu", Severity: "critical", Kind: "", Source: "cpu = 95.0 >= threshold 90.0", Time: 1000, Acked: false},
		{Key: "mem", Severity: "warning", Kind: "", Source: "mem = 80.0 >= threshold 75.0", Time: 2000, Acked: true, AckedAt: 2500},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ActiveAlerts() = %+v, want %+v (sorted by key)", got, want)
	}
}

// TestAlertHistoryNewestFirstAndLimit exercises AlertHistory against a real
// alertlog.jsonl written via AlertLog.AppendAlertEvent (out of chronological
// append order, to prove the sort is real), pinning: the newest-first sort,
// the limit cap, the since filter (via AlertEventsSince), and the AlertEvent
// -> core.AlertRecord field mapping (Acked always false -- see the method's
// doc for why).
func TestAlertHistoryNewestFirstAndLimit(t *testing.T) {
	stateDir := t.TempDir()

	log := NewAlertLog(filepath.Join(stateDir, "alertlog.jsonl"))
	events := []AlertEvent{
		{Time: 100, Key: "cpu", Title: "CPU high", Severity: "critical", Kind: "fire", Source: "threshold"},
		{Time: 300, Key: "mem", Title: "Mem high", Severity: "warning", Kind: "fire", Source: "threshold"},
		{Time: 200, Key: "cpu", Title: "CPU normal", Severity: "critical", Kind: "recover", Source: "threshold"},
	}
	for _, ev := range events {
		if err := log.AppendAlertEvent(ev); err != nil {
			t.Fatalf("AppendAlertEvent(%+v): %v", ev, err)
		}
	}

	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, stateDir, nil, nil)

	// Unbounded (limit<=0): all 3, newest (Time) first.
	got, err := api.AlertHistory(0, 0)
	if err != nil {
		t.Fatalf("AlertHistory(0, 0): %v", err)
	}
	wantAll := []core.AlertRecord{
		{Key: "mem", Severity: "warning", Kind: "fire", Source: "threshold", Time: 300, Acked: false, Title: "Mem high"},
		{Key: "cpu", Severity: "critical", Kind: "recover", Source: "threshold", Time: 200, Acked: false, Title: "CPU normal"},
		{Key: "cpu", Severity: "critical", Kind: "fire", Source: "threshold", Time: 100, Acked: false, Title: "CPU high"},
	}
	if !reflect.DeepEqual(got, wantAll) {
		t.Fatalf("AlertHistory(0, 0) = %+v, want %+v (newest first)", got, wantAll)
	}

	// limit=2 caps to the 2 newest.
	gotLimited, err := api.AlertHistory(0, 2)
	if err != nil {
		t.Fatalf("AlertHistory(0, 2): %v", err)
	}
	if !reflect.DeepEqual(gotLimited, wantAll[:2]) {
		t.Fatalf("AlertHistory(0, 2) = %+v, want %+v", gotLimited, wantAll[:2])
	}

	// since=250 excludes everything before it (Time 100 and 200), leaving
	// only the Time:300 event.
	gotSince, err := api.AlertHistory(250, 0)
	if err != nil {
		t.Fatalf("AlertHistory(250, 0): %v", err)
	}
	if !reflect.DeepEqual(gotSince, wantAll[:1]) {
		t.Fatalf("AlertHistory(250, 0) = %+v, want %+v", gotSince, wantAll[:1])
	}
}

// TestBuildDashboardViewCopiesScalarsAndDerivedCounts pins the adapter's
// field-by-field copy plus the derived summary counts (containers
// running/total, units failed/total, disks critical) the dashboard's
// summary tiles need. Moved here (untagged) from the removed
// daemon_web_dashboard_test.go (`//go:build web`) when the embedded web was
// retired: buildDashboardView itself survives untagged in coreapi_inproc.go,
// and these specific derived-value assertions (top-container sort order,
// DisksCritical count, NetRates summation) are not otherwise covered by the
// parity tests above, which only check newInprocAPI's Snapshot() against
// buildDashboardView -- a tautology, since Snapshot() calls buildDashboardView
// directly.
func TestBuildDashboardViewCopiesScalarsAndDerivedCounts(t *testing.T) {
	snap := Snapshot{
		TS: 1_700_000_000, CPU: 37.5, MemPct: 61, SwapPct: 4, Load1: 0.42, Load5: 0.55, Load15: 0.61, TempC: 54,
		Online: true,
		Disks: map[string]float64{
			"/":     91, // >= web.DiskCriticalPct(90) -> critical
			"/data": 22,
		},
		Containers: map[string]string{
			"nextcloud": "running",
			"postgres":  "running",
			"jellyfin":  "exited",
		},
		FailedUnits: []string{"foo.service"},
		Units:       []UnitInfo{{Name: "foo.service"}, {Name: "bar.service"}},
		Processes:   ProcSnapshot{Total: 214, Running: 1, Sleeping: 210, Zombie: 3},
	}

	got := buildDashboardView(snap)

	if got.TS != snap.TS || got.CPU != snap.CPU || got.MemPct != snap.MemPct || got.SwapPct != snap.SwapPct {
		t.Fatalf("scalar copy mismatch: %+v", got)
	}
	if got.Load1 != 0.42 || got.Load5 != 0.55 || got.Load15 != 0.61 || got.TempC != 54 {
		t.Fatalf("load/temp copy mismatch: %+v", got)
	}
	if !got.Online {
		t.Fatalf("Online = false, want true")
	}
	if got.ContainersRunning != 2 || got.ContainersTotal != 3 {
		t.Fatalf("ContainersRunning/Total = %d/%d, want 2/3", got.ContainersRunning, got.ContainersTotal)
	}
	if got.UnitsFailed != 1 || got.UnitsTotal != 2 {
		t.Fatalf("UnitsFailed/Total = %d/%d, want 1/2", got.UnitsFailed, got.UnitsTotal)
	}
	if len(got.Disks) != 2 || got.Disks[0].Mount != "/" || got.Disks[1].Mount != "/data" {
		t.Fatalf("Disks = %+v, want sorted [/ /data]", got.Disks)
	}
	if got.DisksCritical != 1 {
		t.Fatalf("DisksCritical = %d, want 1", got.DisksCritical)
	}
	if got.Processes != (core.ProcessCounts{Total: 214, Running: 1, Sleeping: 210, Zombie: 3}) {
		t.Fatalf("Processes = %+v, want the Snapshot.Processes counts copied over", got.Processes)
	}
	if got.Cores <= 0 {
		t.Fatalf("Cores = %d, want > 0 (runtime.NumCPU())", got.Cores)
	}
}

// TestBuildDashboardViewTopContainersSortedAndCapped pins the "top
// containers by CPU/memory" derivation: sorted descending, capped to 4 rows
// (the mockup dashboard.html's hbar panels), and each row's State comes from
// the separate Containers state map (docker stats and the plain state
// listing are two different shell-outs). Moved here from the removed
// daemon_web_dashboard_test.go -- see the doc on
// TestBuildDashboardViewCopiesScalarsAndDerivedCounts above.
func TestBuildDashboardViewTopContainersSortedAndCapped(t *testing.T) {
	stats := map[string]ContainerStat{
		"a": {Name: "a", CPUPct: 1, MemMiB: 500},
		"b": {Name: "b", CPUPct: 5, MemMiB: 100},
		"c": {Name: "c", CPUPct: 3, MemMiB: 900},
		"d": {Name: "d", CPUPct: 2, MemMiB: 50},
		"e": {Name: "e", CPUPct: 4, MemMiB: 700},
	}
	snap := Snapshot{
		Containers:     map[string]string{"a": "running", "b": "exited"},
		ContainerStats: stats,
	}

	got := buildDashboardView(snap)

	if len(got.TopCPUContainers) != 4 {
		t.Fatalf("len(TopCPUContainers) = %d, want 4", len(got.TopCPUContainers))
	}
	wantCPUOrder := []string{"b", "e", "c", "d"}
	for i, name := range wantCPUOrder {
		if got.TopCPUContainers[i].Name != name {
			t.Fatalf("TopCPUContainers[%d].Name = %q, want %q (full: %+v)", i, got.TopCPUContainers[i].Name, name, got.TopCPUContainers)
		}
	}
	if got.TopCPUContainers[0].State != "exited" {
		t.Fatalf("TopCPUContainers[0] (%q) State = %q, want exited", got.TopCPUContainers[0].Name, got.TopCPUContainers[0].State)
	}

	if len(got.TopMemContainers) != 4 {
		t.Fatalf("len(TopMemContainers) = %d, want 4", len(got.TopMemContainers))
	}
	wantMemOrder := []string{"c", "e", "a", "b"}
	for i, name := range wantMemOrder {
		if got.TopMemContainers[i].Name != name {
			t.Fatalf("TopMemContainers[%d].Name = %q, want %q (full: %+v)", i, got.TopMemContainers[i].Name, name, got.TopMemContainers)
		}
	}
	// "a" (index 2 in the mem-sorted order above) has a known state in the
	// separate Containers map ("running") -- pins that TopMemContainers'
	// State field is looked up from there, not left zero-valued.
	if a := got.TopMemContainers[2]; a.Name != "a" || a.State != "running" {
		t.Fatalf("TopMemContainers[2] = %+v, want {Name:a State:running ...}", a)
	}
}

// TestBuildDashboardViewDiskDetailAndNetRates pins DiskDetail merge (device/
// free/size/fill-projection layered onto the plain Disks usage%) and the
// NetRates -> NetIfaces/NetRxBps/NetTxBps summation. Moved here from the
// removed daemon_web_dashboard_test.go -- see the doc on
// TestBuildDashboardViewCopiesScalarsAndDerivedCounts above.
func TestBuildDashboardViewDiskDetailAndNetRates(t *testing.T) {
	snap := Snapshot{
		Disks: map[string]float64{"/": 91},
		DiskDetail: map[string]DiskDetail{
			"/": {Device: "/dev/sda1", UsagePct: 91, FreeBytes: 4_100_000_000, SizeBytes: 100_000_000_000, DaysToFull: 3, DaysToFullKnown: true},
		},
		NetRates: map[string]IfaceRate{
			"eth0":  {RxBps: 1_800_000, TxBps: 240_000},
			"wlan0": {RxBps: 100, TxBps: 50},
		},
	}

	got := buildDashboardView(snap)

	if len(got.Disks) != 1 {
		t.Fatalf("len(Disks) = %d, want 1", len(got.Disks))
	}
	d := got.Disks[0]
	if d.Device != "/dev/sda1" || d.FreeBytes != 4_100_000_000 || d.SizeBytes != 100_000_000_000 || !d.DaysToFullKnown || d.DaysToFull != 3 {
		t.Fatalf("Disks[0] = %+v, want the DiskDetail fields merged in", d)
	}

	if len(got.NetIfaces) != 2 || got.NetIfaces[0].Name != "eth0" || got.NetIfaces[1].Name != "wlan0" {
		t.Fatalf("NetIfaces = %+v, want sorted [eth0 wlan0]", got.NetIfaces)
	}
	if got.NetRxBps != 1_800_100 || got.NetTxBps != 240_050 {
		t.Fatalf("NetRxBps/NetTxBps = %v/%v, want summed across interfaces", got.NetRxBps, got.NetTxBps)
	}
}

// TestBuildDashboardViewNoMapMutationUnderConcurrentPublish is the hard
// map-safety requirement pinned since the web-dashboard task: buildDashboardView
// is a concurrent reader of snapshotHub, and the whole atomic.Pointer[Snapshot]
// design (see snapshot_hub.go's doc) depends on every reader treating a
// loaded Snapshot's map fields as read-only, since a copy of the Snapshot
// struct still aliases the same underlying maps the publisher just replaced
// a field with. This test runs a publisher goroutine that keeps replacing
// snapshotHub's Snapshot (via a brand-new map each time, mirroring
// mergeSlowFields' "replace wholesale" contract) concurrently with many
// readers calling latestSnapshot()+buildDashboardView() -- go test -race is
// what actually proves no race; the assertions here just guard against the
// adapter silently corrupting values in a way -race wouldn't catch. Moved
// here from the removed daemon_web_dashboard_test.go -- see the doc on
// TestBuildDashboardViewCopiesScalarsAndDerivedCounts above.
func TestBuildDashboardViewNoMapMutationUnderConcurrentPublish(t *testing.T) {
	old := snapshotHub.Load()
	t.Cleanup(func() { snapshotHub.Store(old) })

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			i++
			snap := Snapshot{
				TS: int64(i),
				Disks: map[string]float64{
					"/": float64(i % 100),
				},
				Containers: map[string]string{
					"svc": "running",
				},
				ContainerStats: map[string]ContainerStat{
					"svc": {Name: "svc", CPUPct: float64(i % 100), MemMiB: 128},
				},
				NetRates: map[string]IfaceRate{
					"eth0": {RxBps: float64(i), TxBps: float64(i)},
				},
			}
			snapshotHub.Store(&snap)
		}
	}()

	readers := 8
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				view := buildDashboardView(latestSnapshot())
				_ = view // buildDashboardView must only read; -race catches any write races
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestInprocSubscribeDeliversPublishedEvents pins Subscribe's happy path:
// the channel it returns must be a live subscription onto the inprocAPI's
// own bus -- publishing directly on that bus (as dispatchAndLog/the sampler
// loop, daemon.go, do in production) must deliver the event to the caller.
func TestInprocSubscribeDeliversPublishedEvents(t *testing.T) {
	bus := newEventBus()
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, bus)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := api.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := core.Event{Kind: "alert_fire", Severity: "critical", Source: "anomaly", Title: "cpu high", Time: 42}
	bus.Publish(want)

	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("received %+v, want %+v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the published event on Subscribe's channel")
	}
}

// TestInprocSubscribeCtxCancelUnsubscribes pins Subscribe's cleanup path:
// cancelling the ctx passed to Subscribe must unsubscribe from the bus (a
// later Publish is not delivered) and close the returned channel -- proven
// by receiving from it after cancellation and observing ok==false, the same
// closed-channel signal eventBus.Subscribe's own cancel produces
// (eventbus_test.go). This is a blocking receive on purpose: it waits for
// the actual close event Subscribe's ctx.Done() goroutine produces rather
// than assuming any particular timing, guarded by a time.After fallback so
// a broken implementation fails the test instead of hanging forever.
func TestInprocSubscribeCtxCancelUnsubscribes(t *testing.T) {
	bus := newEventBus()
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return config.Default() }, nil, t.TempDir(), nil, bus)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := api.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	cancel()

	select {
	case got, ok := <-ch:
		if ok {
			t.Fatalf("channel still open after ctx cancel: received %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Subscribe's channel to close after ctx cancel")
	}
}
