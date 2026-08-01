package serverwatch

import (
	"reflect"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// TestInprocSnapshotProjectsScalars is the Step 1 failing test from the
// task-4 brief: newInprocAPI's Snapshot() must project a Snapshot's scalar
// fields into a core.DashboardView, same as buildDashboardView already does
// for the web build.
func TestInprocSnapshotProjectsScalars(t *testing.T) {
	snap := Snapshot{TS: 42, CPU: 12.5, MemPct: 30, Online: true}
	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return config.Default() }, nil, t.TempDir())
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

	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return cfg }, nil, t.TempDir())
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

	api := newInprocAPI(func() Snapshot { return snap }, func() *config.Config { return cfg }, nil, t.TempDir())
	got, err := api.Monitoring()
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("newInprocAPI(...).Monitoring() != buildMonitoringView(snap, cfg):\ngot:  %+v\nwant: %+v", got, want)
	}
}
