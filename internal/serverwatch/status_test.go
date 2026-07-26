package serverwatch

import (
	"encoding/json"
	"reflect"
	"testing"
)

// newFullSnapshot returns a Snapshot with every extended-collection field
// from Epic #69 populated (ContainerStats, NetRates, Units, Processes,
// DiskDetail, SmartAttrs), for TestSnapshotJSONRoundTripsExtendedFields.
func newFullSnapshot() Snapshot {
	return Snapshot{
		TS:           1700000000,
		CPU:          12.5,
		MemPct:       55,
		SwapPct:      2,
		Load1:        1.1,
		Load5:        0.9,
		Load15:       0.7,
		TempC:        45,
		Disks:        map[string]float64{"/": 70},
		Online:       true,
		DockerAccess: "socket",
		Containers:   map[string]string{"web": "running"},
		FailedUnits:  []string{"nginx.service"},
		SmartHealth:  map[string]string{"/dev/sda": "PASSED"},
		DiskDetail: map[string]DiskDetail{
			"/": {
				Device: "/dev/sda1", FsType: "ext4",
				UsagePct: 70, InodePct: 40,
				FreeBytes: 1024, SizeBytes: 4096,
				DaysToFull: 12.5, DaysToFullKnown: true,
			},
		},
		SmartAttrs: map[string]SmartAttr{
			"/dev/sda": {TempC: 37, WearPct: 95, ReallocSectors: 2},
		},
		ContainerStats: map[string]ContainerStat{
			"web": {Name: "web", CPUPct: 3.5, MemMiB: 128, NetRxMB: 1.2, NetTxMB: 0.4},
		},
		NetRates: map[string]IfaceRate{
			"eth0": {RxBps: 1234.5, TxBps: 678.9},
		},
		Units: []UnitInfo{
			{Name: "nginx.service", Load: "loaded", Active: "active", Sub: "running", Description: "A high performance web server"},
		},
		Processes: ProcSnapshot{
			Total: 120, Running: 2, Sleeping: 115, Zombie: 1,
			Top: []ProcInfo{
				{PID: 1234, Name: "nginx", State: "S", CPUPct: 5.5, MemMiB: 32, Threads: 4},
			},
		},
	}
}

// TestSnapshotJSONRoundTripsExtendedFields marshals a fully-populated
// Snapshot to JSON and unmarshals it back, asserting every extended-
// collection field (issue #77, Epic #69) survives intact. This is the
// contract status.json makes with the UI: Store.WriteStatus (store.go) does
// a plain json.MarshalIndent of whatever Snapshot the daemon hands it, so
// any field that doesn't round-trip here wouldn't be readable from
// status.json either.
func TestSnapshotJSONRoundTripsExtendedFields(t *testing.T) {
	want := newFullSnapshot()

	b, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if !reflect.DeepEqual(want.ContainerStats, got.ContainerStats) {
		t.Errorf("ContainerStats round-trip = %+v, want %+v", got.ContainerStats, want.ContainerStats)
	}
	if !reflect.DeepEqual(want.Units, got.Units) {
		t.Errorf("Units round-trip = %+v, want %+v", got.Units, want.Units)
	}
	if !reflect.DeepEqual(want.Processes, got.Processes) {
		t.Errorf("Processes round-trip = %+v, want %+v", got.Processes, want.Processes)
	}
	if !reflect.DeepEqual(want.NetRates, got.NetRates) {
		t.Errorf("NetRates round-trip = %+v, want %+v", got.NetRates, want.NetRates)
	}
	if !reflect.DeepEqual(want.DiskDetail, got.DiskDetail) {
		t.Errorf("DiskDetail round-trip = %+v, want %+v", got.DiskDetail, want.DiskDetail)
	}
	if !reflect.DeepEqual(want.SmartAttrs, got.SmartAttrs) {
		t.Errorf("SmartAttrs round-trip = %+v, want %+v", got.SmartAttrs, want.SmartAttrs)
	}
	// Full-struct compare as a belt-and-braces catch-all for any other field
	// (including the pre-existing ones) silently losing a json tag/type.
	if !reflect.DeepEqual(want, got) {
		t.Errorf("full Snapshot round-trip mismatch:\n got  = %+v\n want = %+v", got, want)
	}
}

// slowMergeExcludedFields lists the Snapshot fields mergeSlowFields
// deliberately does NOT copy, and why, so
// TestMergeSlowFieldsCopiesEverySlowTierField can tell "intentionally
// excluded" apart from "forgotten":
//   - TS, CPU, MemPct, SwapPct, Load1, Load5, Load15, TempC: fast-tier fields,
//     collectSlow never sets them (collectFast does).
//   - NetRates, Processes: populated by the caller directly from stateful
//     calculators (NetRateCalc/ProcCPUCalc) that collectSlow has no access
//     to, not by collectSlow itself — see mergeSlowFields's doc comment.
var slowMergeExcludedFields = map[string]bool{
	"TS": true, "CPU": true, "MemPct": true, "SwapPct": true,
	"Load1": true, "Load5": true, "Load15": true, "TempC": true,
	"NetRates": true, "Processes": true,
}

// TestMergeSlowFieldsCopiesEverySlowTierField guards the exact bug class
// issue #77 asks for: a new Snapshot field that collectSlow populates but
// that mergeSlowFields (shared by cmdDaemon's sampler loop and
// collectSnapshot, daemon.go) forgets to copy, which would silently vanish
// from status.json between slow ticks. It works by reflecting over every
// field of a fully-populated Snapshot (newFullSnapshot) and asserting
// mergeSlowFields copied it onto an empty merged Snapshot, for every field
// not in the explicit, documented exclusion list above. Adding a Snapshot
// field without updating either mergeSlowFields or
// slowMergeExcludedFields makes this test fail with the specific field name,
// rather than the gap going unnoticed.
func TestMergeSlowFieldsCopiesEverySlowTierField(t *testing.T) {
	slow := newFullSnapshot()
	var merged Snapshot
	mergeSlowFields(&merged, slow)

	mergedV := reflect.ValueOf(merged)
	slowV := reflect.ValueOf(slow)
	typ := mergedV.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if slowMergeExcludedFields[name] {
			continue
		}
		got := mergedV.Field(i).Interface()
		want := slowV.Field(i).Interface()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("mergeSlowFields did not copy Snapshot.%s: merged = %+v, want %+v (from slow snapshot) — "+
				"if this field is genuinely not collectSlow's to set, add it to slowMergeExcludedFields instead",
				name, got, want)
		}
	}
}
