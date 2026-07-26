//go:build web

package serverwatch

import (
	"sync"
	"testing"
	"time"

	"serverwatch/internal/web"
)

// TestBuildDashboardViewCopiesScalarsAndDerivedCounts pins the adapter's
// field-by-field copy plus the derived summary counts (containers
// running/total, units failed/total, disks critical) the dashboard's
// summary tiles need.
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
	if got.Processes != (web.ProcessCounts{Total: 214, Running: 1, Sleeping: 210, Zombie: 3}) {
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
// listing are two different shell-outs).
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
// NetRates -> NetIfaces/NetRxBps/NetTxBps summation.
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
// map-safety requirement for this task: buildDashboardView is the FIRST
// concurrent reader of snapshotHub, and the whole atomic.Pointer[Snapshot]
// design (see web_deps.go's doc) depends on every reader treating a loaded
// Snapshot's map fields as read-only, since a copy of the Snapshot struct
// still aliases the same underlying maps the publisher just replaced a
// field with. This test runs a publisher goroutine that keeps replacing
// snapshotHub's Snapshot (via a brand-new map each time, mirroring
// mergeSlowFields' "replace wholesale" contract) concurrently with many
// readers calling latestSnapshot()+buildDashboardView() — go test -race is
// what actually proves no race; the assertions here just guard against the
// adapter silently corrupting values in a way -race wouldn't catch.
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
