package trinetra

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// newFullSnapshot returns a Snapshot with every extended-collection field from Epic #69
// populated (ContainerStats, NetRates, Units, Processes, DiskDetail, SmartAttrs).
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
		CollectorErrors: map[string]string{"docker": "timeout"},
		CollectorHealth: map[string]CollectorStat{
			"docker": {Fails: 2, LastSuccessUnix: 1699999900, LastError: "timeout"},
		},
	}
}

// TestSnapshotJSONRoundTripsExtendedFields marshals a fully-populated Snapshot to JSON and
// unmarshals it back, asserting every extended- collection field.
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

// slowMergeExcludedFields lists the Snapshot fields mergeSlowFields deliberately does NOT
// copy, and why.
var slowMergeExcludedFields = map[string]bool{
	"TS": true, "CPU": true, "MemPct": true, "SwapPct": true,
	"Load1": true, "Load5": true, "Load15": true, "TempC": true,
	"NetRates": true, "Processes": true, "SlowStale": true,
	"CollectorErrors": true,
}

// TestMergeSlowFieldsCopiesEverySlowTierField guards the exact bug class issue #77 asks
// for: a new Snapshot field that collectSlow populates but that mergeSlowFields.
func TestMergeSlowFieldsCopiesEverySlowTierField(t *testing.T) {
	slow := newFullSnapshot()
	var merged Snapshot
	mergeSlowFields(&merged, slow)

	mergedV := reflect.ValueOf(merged)
	slowV := reflect.ValueOf(slow)
	typ := mergedV.Type()
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if typ.Field(i).PkgPath != "" {
			continue // unexported (e.g. collectorsAttempted): not reflectable, internal handoff only
		}
		if slowMergeExcludedFields[name] {
			continue
		}
		got := mergedV.Field(i).Interface()
		want := slowV.Field(i).Interface()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("mergeSlowFields did not copy Snapshot.%s: merged = %+v, want %+v (from slow snapshot) -- "+
				"if this field is genuinely not collectSlow's to set, add it to slowMergeExcludedFields instead",
				name, got, want)
		}
	}
}

// TestRenderStatusAllClear asserts the redesigned /stats,/status overview: a header, a
// <pre> resource table, one summary-count line per category.
func TestRenderStatusAllClear(t *testing.T) {
	s := Snapshot{
		CPU: 10, MemPct: 20, SwapPct: 0, Load1: 0.1, TempC: 40,
		Online:      true,
		Disks:       map[string]float64{"/": 30, "/boot": 5},
		Containers:  map[string]string{"web": "running"},
		SmartHealth: map[string]string{"/dev/sda": "PASSED"},
	}
	r := renderStatus(s, config.Default())
	if !strings.Contains(r, "✅ all clear") {
		t.Fatalf("want header all clear, got %q", r)
	}
	if !strings.Contains(r, "<pre>") || !strings.Contains(r, "</pre>") {
		t.Fatalf("want a <pre> resource table, got %q", r)
	}
	if !strings.Contains(r, "Disks: 2 ok / 0 warn / 0 CRIT (of 2)") {
		t.Fatalf("want disk summary counts, got %q", r)
	}
	if !strings.Contains(r, "Docker: 1/1 running (0 down)") {
		t.Fatalf("want docker summary counts, got %q", r)
	}
	if !strings.Contains(r, "systemd: 0 failed") {
		t.Fatalf("want systemd summary, got %q", r)
	}
	if !strings.Contains(r, "SMART: 1 ok / 0 FAILED") {
		t.Fatalf("want smart summary, got %q", r)
	}
	if !strings.Contains(r, "Internet: up") {
		t.Fatalf("want internet summary, got %q", r)
	}
	if !strings.Contains(r, "✅ all systems normal") {
		t.Fatalf("want the all-systems-normal closing line, got %q", r)
	}
	if strings.Contains(r, "<b>Disks:</b>") || strings.Contains(r, "<b>Docker") {
		t.Fatalf("must NOT enumerate healthy mounts/containers, got %q", r)
	}
}

// TestRenderStatusOnlyFailures asserts that when something IS failing, the header reflects
// severity and the detail section lists ONLY the failing disk/container/unit/smart entries.
func TestRenderStatusOnlyFailures(t *testing.T) {
	c := config.Default() // Thresholds.DiskPct = 90 by default
	s := Snapshot{
		Online: true,
		Disks:  map[string]float64{"/": 95, "/boot": 10}, // / over threshold, /boot healthy
		Containers: map[string]string{
			"web": "running", "db": "exited",
		},
		FailedUnits: []string{"nginx.service"},
		SmartHealth: map[string]string{"/dev/sda": "FAILED", "/dev/sdb": "PASSED"},
	}
	r := renderStatus(s, c)
	if !strings.Contains(r, "❌") {
		t.Fatalf("want a critical marker in the header, got %q", r)
	}
	if !strings.Contains(r, "/ 95%") {
		t.Fatalf("want the failing disk listed with its %%, got %q", r)
	}
	if strings.Contains(r, "<b>Disks:</b>\n  ") && strings.Contains(r, "/boot 10%") {
		t.Fatalf("must not list the healthy /boot mount in the failure detail, got %q", r)
	}
	if !strings.Contains(r, "db") || !strings.Contains(r, "exited") {
		t.Fatalf("want the down container listed, got %q", r)
	}
	if !strings.Contains(r, "nginx.service") {
		t.Fatalf("want the failed unit listed, got %q", r)
	}
	if !strings.Contains(r, "/dev/sda") {
		t.Fatalf("want the FAILED smart device listed, got %q", r)
	}
	if strings.Contains(r, "/dev/sdb") {
		t.Fatalf("must not list the healthy /dev/sdb smart device in failure detail, got %q", r)
	}
}

// TestRenderStatusCapsFailureDetailLists asserts a long list of failures (e.g. many down
// containers) is bounded to a sane max with a "+N more" suffix.
func TestRenderStatusCapsFailureDetailLists(t *testing.T) {
	containers := map[string]string{}
	for i := 0; i < 15; i++ {
		containers[fmt.Sprintf("app%02d", i)] = "exited"
	}
	s := Snapshot{Online: true, Containers: containers}
	r := renderStatus(s, config.Default())
	if !strings.Contains(r, "+5 more") {
		t.Fatalf("want a capped list with '+5 more' suffix (15 down, cap 10), got %q", r)
	}
}

// TestRenderStatusEscapesHTML asserts dynamic content (container/unit/mount names) is
// HTML-escaped, since SendMessage now sends with parse_mode=HTML.
func TestRenderStatusEscapesHTML(t *testing.T) {
	s := Snapshot{
		Online:      true,
		Containers:  map[string]string{"web<script>": "exited"},
		FailedUnits: []string{"a&b.service"},
	}
	r := renderStatus(s, config.Default())
	if strings.Contains(r, "<script>") {
		t.Fatalf("container name must be HTML-escaped, got %q", r)
	}
	if !strings.Contains(r, "&lt;script&gt;") {
		t.Fatalf("want escaped container name, got %q", r)
	}
	if strings.Contains(r, "a&b.service") {
		t.Fatalf("unit name must be HTML-escaped, got %q", r)
	}
	if !strings.Contains(r, "a&amp;b.service") {
		t.Fatalf("want escaped unit name, got %q", r)
	}
}

// TestRenderStatusNilConfigDoesNotPanic guards handleCommand's nil-config degrade path
// (mirrors the existing nil-store guard style in this package).
func TestRenderStatusNilConfigDoesNotPanic(t *testing.T) {
	s := Snapshot{CPU: 50, Online: true}
	r := renderStatus(s, nil)
	if !strings.Contains(r, "CPU") {
		t.Fatalf("nil-config render = %q, want it to still render", r)
	}
}

// TestRenderDisksTableSortedAndCapped asserts /disk's redesigned compact table: a summary
// count line, mounts sorted by use%% descending, free space shown from DiskDetail.
func TestRenderDisksTableSortedAndCapped(t *testing.T) {
	disks := map[string]float64{"/": 10, "/boot": 90, "/mnt/data": 50}
	detail := map[string]DiskDetail{
		"/boot": {FreeBytes: 1024},
	}
	r := renderDisks(disks, detail)
	if !strings.Contains(r, "3 filesystems") {
		t.Fatalf("want a summary count line, got %q", r)
	}
	iBoot := strings.Index(r, "/boot")
	iData := strings.Index(r, "/mnt/data")
	iRoot := strings.Index(r, "/ ")
	if iBoot == -1 || iData == -1 || iRoot == -1 {
		t.Fatalf("want all three mounts listed, got %q", r)
	}
	if !(iBoot < iData && iData < iRoot) {
		t.Fatalf("want mounts sorted by use%% desc (boot 90 > data 50 > root 10), got %q", r)
	}
	if !strings.Contains(r, "1.0KiB") && !strings.Contains(r, "1.0K") {
		t.Fatalf("want /boot's free bytes rendered human-readable, got %q", r)
	}

	// Cap test: more than 15 real mounts still gets bounded.
	many := map[string]float64{}
	for i := 0; i < 20; i++ {
		many[fmt.Sprintf("/m%02d", i)] = float64(i)
	}
	r2 := renderDisks(many, nil)
	if !strings.Contains(r2, "+5 more") {
		t.Fatalf("want a '+5 more' cap suffix for 20 mounts, got %q", r2)
	}
}
