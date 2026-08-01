package serverwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// TestBuildDoctorReport is the focused TDD test the task-7 brief specifies:
// buildDoctorReport must report the discovered-target count and collector
// toggles for a fake Exec/FileSource, without touching the real host.
func TestBuildDoctorReport(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "smartctl" {
			return []byte("/dev/sda -d ata # /dev/sda, ATA device\n"), nil
		}
		// docker, sudo, df, ...: simulate "command not available".
		return nil, errNotExist
	}}
	fs := fakeFS{globs: map[string][]string{
		"/sys/class/thermal/thermal_zone*/temp": {"/sys/class/thermal/thermal_zone0/temp"},
	}}
	c := config.Default()

	rep := buildDoctorReport(x, fs, c, nil)

	if rep.TargetsDiscovered < 0 {
		t.Fatalf("bad report: %+v", rep)
	}
	if rep.DockerAccess != "available=false method=" {
		t.Errorf("DockerAccess = %q, want %q", rep.DockerAccess, "available=false method=")
	}
	if !rep.SmartctlAvailable {
		t.Error("SmartctlAvailable = false, want true (fake smartctl --scan succeeded)")
	}
	if rep.ThermalZones != 1 {
		t.Errorf("ThermalZones = %d, want 1", rep.ThermalZones)
	}
	// docker unavailable still yields one "docker (unavailable)" target, and
	// the thermal zone yields a "temp" target, so at least 2 are expected.
	if rep.TargetsDiscovered < 2 {
		t.Errorf("TargetsDiscovered = %d, want >= 2", rep.TargetsDiscovered)
	}
	if !rep.ContainerStatsOn || !rep.NetThroughputOn || !rep.ServicesOn || !rep.ProcessesOn || !rep.SmartAttrsOn {
		t.Errorf("collector toggles = %+v, want all on for config.Default()", rep)
	}
	if rep.StoreStats != "unavailable" {
		t.Errorf("StoreStats = %q, want %q (nil store)", rep.StoreStats, "unavailable")
	}
}

// TestBuildDoctorReportStoreStats asserts a live store's Stats() feed
// through into StoreStats with the same "N series, X.X MB on disk (raw+1m)"
// wording collectorSummary has always used.
func TestBuildDoctorReportStoreStats(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(1, MetricSet{"cpu": 1, "mem": 2}); err != nil {
		t.Fatal(err)
	}

	rep := buildDoctorReport(fakeExec{fn: func(string, ...string) ([]byte, error) {
		return nil, errNotExist
	}}, fakeFS{}, config.Default(), store)

	if !strings.Contains(rep.StoreStats, "2 series") {
		t.Errorf("StoreStats = %q, want it to mention 2 series", rep.StoreStats)
	}
}

// TestRenderDoctorReport is the golden test for the print format
// cmdDoctor has always emitted: given a hand-built core.DoctorReport, it
// pins the exact byte-for-byte output, independent of buildDoctorReport's
// probe logic or the host this test runs on.
func TestRenderDoctorReport(t *testing.T) {
	rep := core.DoctorReport{
		DockerAccess:      "available=true method=socket",
		SmartctlAvailable: true,
		ThermalZones:      2,
		TargetsDiscovered: 5,
		ContainerStatsOn:  true,
		NetThroughputOn:   false,
		ServicesOn:        true,
		ProcessesOn:       false,
		SmartAttrsOn:      true,
		StoreStats:        "3 series, 1.2 MB on disk (raw+1m)",
	}

	var buf bytes.Buffer
	renderDoctorReport(&buf, rep)

	want := "docker: available=true method=socket\n" +
		"smartctl: ok\n" +
		"thermal zones: 2\n" +
		"targets discovered: 5\n" +
		"collectors: container_stats=on net_throughput=off services=on processes=off smart_attrs=on\n" +
		"time-series: 3 series, 1.2 MB on disk (raw+1m)\n"
	if buf.String() != want {
		t.Errorf("renderDoctorReport =\n%s\nwant\n%s", buf.String(), want)
	}
}

// TestRenderDoctorReportUnavailable pins the "smartctl: unavailable" /
// "time-series: unavailable" branches, the counterparts to the "ok" /
// store-stats branches TestRenderDoctorReport covers.
func TestRenderDoctorReportUnavailable(t *testing.T) {
	rep := core.DoctorReport{
		DockerAccess: "available=false method=",
		StoreStats:   "unavailable",
	}

	var buf bytes.Buffer
	renderDoctorReport(&buf, rep)

	want := "docker: available=false method=\n" +
		"smartctl: unavailable\n" +
		"thermal zones: 0\n" +
		"targets discovered: 0\n" +
		"collectors: container_stats=off net_throughput=off services=off processes=off smart_attrs=off\n" +
		"time-series: unavailable\n"
	if buf.String() != want {
		t.Errorf("renderDoctorReport =\n%s\nwant\n%s", buf.String(), want)
	}
}

// TestCmdDoctorOutputUnchanged is the CLI-level golden test the task-7
// brief's behavior-preservation gate calls for: it runs the real `doctor`
// command (real osExec{}/osFS{}, same as before this refactor) and checks
// every line renderDoctorReport now produces is present with the exact same
// wording cmdDoctor printed inline before extraction -- guarding against the
// refactor silently changing cmdDoctor's output shape. Docker/smartctl/
// thermal-zone availability itself is host-dependent, so this only pins the
// literal, host-independent parts of each line (labels/format), the same
// scope TestCmdDoctorPrintsCollectorSummary already covers for the
// collectors/time-series lines.
func TestCmdDoctorOutputUnchanged(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d", code)
	}
	got := out.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("doctor printed %d lines, want 6; got:\n%s", len(lines), got)
	}
	wantPrefixes := []string{
		"docker: available=",
		"smartctl: ",
		"thermal zones: ",
		"targets discovered: ",
		"collectors: container_stats=",
		"time-series: ",
	}
	for i, prefix := range wantPrefixes {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
}

// TestDoctorInprocAPI asserts inprocAPI.Doctor() (the core.API entry point
// added by this task) delegates to buildDoctorReport and returns no error,
// using the live store held by the API rather than opening a fresh one.
func TestDoctorInprocAPI(t *testing.T) {
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(1, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	api := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, store, t.TempDir())

	rep, err := api.Doctor()
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if !strings.Contains(rep.StoreStats, "1 series") {
		t.Errorf("StoreStats = %q, want it to mention 1 series (live store)", rep.StoreStats)
	}
}

// TestDoctorFileAPI asserts fileAPI.Doctor() opens the configured store
// fresh and reports its stats, mirroring cmdDoctor's own store-open path.
func TestDoctorFileAPI(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	if err := cfg.Set("storage.backend", "memory"); err != nil {
		t.Fatal(err)
	}
	stateDir = dir

	api := newFileAPI(dir, cfg)
	rep, err := api.Doctor()
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if rep.StoreStats == "" {
		t.Error("StoreStats = \"\", want a non-empty value")
	}
}

// TestDoctorFileAPINilStoreDegrades asserts a store that fails to open
// degrades Doctor() to StoreStats "unavailable" rather than returning an
// error -- read-only diagnostics shouldn't fail over, the same reasoning
// cmdDoctor's own corrupt-config/store-open fallback already applies.
func TestDoctorFileAPINilStoreDegrades(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	if err := cfg.Set("storage.backend", "tsfile"); err != nil {
		t.Fatal(err)
	}
	// openConfiguredStore always opens the backend rooted at the
	// package-level stateDir (migrate.go), not a config field -- point that
	// at a regular file (rather than a directory) so tsfile's MkdirAll
	// fails and Doctor() must degrade instead of erroring.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir = blocked

	api := newFileAPI(dir, cfg)
	rep, err := api.Doctor()
	if err != nil {
		t.Fatalf("Doctor() error = %v, want nil (degrade instead)", err)
	}
	if rep.StoreStats != "unavailable" {
		t.Errorf("StoreStats = %q, want %q", rep.StoreStats, "unavailable")
	}
}
