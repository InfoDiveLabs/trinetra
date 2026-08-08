package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// fakeAPI is a core.API stub returning canned values so run() (and, in
// tui_test.go, the TUI model) can be driven without a real control socket.
// Only the reads the ctl subcommands/wizard exercise are populated by
// default; the rest satisfy the interface and are never called by most
// tested paths. cfg/applyErr let a test control what Config() returns and
// how ApplyConfig fails; applied/applyCalls record what a caller (the web
// setup wizard) actually posted, for assertions.
type fakeAPI struct {
	snapshot core.DashboardView
	doctor   core.DoctorReport
	hostInfo      core.HostInfoView
	containerLogs string
	logErr        error
	version       string
	active   []core.AlertRecord

	cfg       *config.Config
	configErr error
	applyErr  error
	applied   *config.Config
	applyN    int

	// monitorTargets/monitorTargetsErr back MonitorTargets, so the monitor-
	// thresholds screen's tests (manage_ui_test.go) can drive it without a
	// real socket or real docker/df/smartctl discovery.
	monitorTargets    []core.TargetView
	monitorTargetsErr error

	// validateErr/validateCalls back ValidateChannel for the Channels
	// screen's #79-safe validate-before-save gate tests (channels_test.go,
	// manage_channels_test.go): validateErr controls whether the gate
	// passes or fails, validateCalls records every cc it was asked to check
	// so a test can assert the gate was (or wasn't) actually consulted.
	validateErr   error
	validateCalls []config.ChannelConfig

	// testChannelErr/testChannelCalls back TestChannel for the Channels
	// screen's "test" action (manage_channels_test.go).
	testChannelErr   error
	testChannelCalls []string

	// enrollPIN/enrollEnrolled/enrollErr back EnrollmentPIN for the
	// first-run onboarding flow's tests (onboarding_test.go,
	// onboard_ui_test.go): enrollCalls records how many times it was
	// polled, so a test can assert the poll loop actually re-fetches.
	enrollPIN      string
	enrollEnrolled bool
	enrollErr      error
	enrollCalls    int
}

func (f *fakeAPI) Snapshot() (core.DashboardView, error) { return f.snapshot, nil }
func (f *fakeAPI) Monitoring() (core.MonitoringView, error) {
	return core.MonitoringView{}, nil
}
func (f *fakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	return nil, nil
}
func (f *fakeAPI) Events(from, to int64) ([]core.DownEventView, error) { return nil, nil }
func (f *fakeAPI) ActiveAlerts() ([]core.AlertRecord, error)           { return f.active, nil }
func (f *fakeAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return nil, nil
}
func (f *fakeAPI) Config() (*config.Config, error) {
	if f.configErr != nil {
		return nil, f.configErr
	}
	if f.cfg != nil {
		return f.cfg, nil
	}
	return &config.Config{}, nil
}
func (f *fakeAPI) Doctor() (core.DoctorReport, error) { return f.doctor, nil }
func (f *fakeAPI) HostInfo() (core.HostInfoView, error) { return f.hostInfo, nil }
func (f *fakeAPI) ContainerLogs(name string, lines int) (string, error) {
	return f.containerLogs, f.logErr
}
func (f *fakeAPI) Version() (string, error) { return f.version, nil }

// EnrollmentPIN returns the canned enrollPIN/enrollEnrolled/enrollErr a test
// set up, recording every call in enrollCalls so onboarding's poll-until-
// enrolled loop (onboard_ui.go) can be asserted to actually re-fetch rather
// than just checking the first result forever.
func (f *fakeAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) {
	f.enrollCalls++
	if f.enrollErr != nil {
		return "", false, f.enrollErr
	}
	return f.enrollPIN, f.enrollEnrolled, nil
}
func (f *fakeAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return f.monitorTargets, f.monitorTargetsErr
}
func (f *fakeAPI) ApplyConfig(c *config.Config) error {
	f.applyN++
	f.applied = c
	return f.applyErr
}
func (f *fakeAPI) AckAlert(key string) error   { return nil }
func (f *fakeAPI) UnackAlert(key string) error { return nil }

// TestChannel records name in testChannelCalls and returns testChannelErr,
// so the Channels screen's "test" action (manage_channels.go) can be
// asserted against without a real notifier send.
func (f *fakeAPI) TestChannel(name string) error {
	f.testChannelCalls = append(f.testChannelCalls, name)
	return f.testChannelErr
}

// ValidateChannel records cc in validateCalls and returns validateErr, so
// the Channels screen's #79-safe validate-before-save gate (saveChannel,
// channels.go) can be asserted to have (or not have) actually consulted it.
func (f *fakeAPI) ValidateChannel(cc config.ChannelConfig) error {
	f.validateCalls = append(f.validateCalls, cc)
	return f.validateErr
}

func (f *fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, nil
}

func TestRunStatus(t *testing.T) {
	api := &fakeAPI{snapshot: core.DashboardView{
		TS:              1700000000,
		Online:          true,
		CPU:             42.5,
		MemPct:          63.25,
		Cores:           8,
		ContainersTotal: 3,
		UnitsFailed:     1,
	}}
	var buf bytes.Buffer
	if code := run(api, []string{"status"}, &buf); code != 0 {
		t.Fatalf("run status exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"42.5", "63.2", "online", "8", "cores"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q\n%s", want, out)
		}
	}
}

func TestRunDoctor(t *testing.T) {
	api := &fakeAPI{doctor: core.DoctorReport{
		DockerAccess:      "available=true method=socket",
		SmartctlAvailable: true,
		ThermalZones:      2,
		TargetsDiscovered: 5,
		ServicesOn:        true,
		StoreStats:        "42 series, 1.2 MB on disk (raw+1m)",
	}}
	var buf bytes.Buffer
	if code := run(api, []string{"doctor"}, &buf); code != 0 {
		t.Fatalf("run doctor exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"available=true method=socket", "42 series", "5"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q\n%s", want, out)
		}
	}
}

func TestRunHost(t *testing.T) {
	api := &fakeAPI{hostInfo: core.HostInfoView{
		Hostname: "attic-pi", OS: "Debian GNU/Linux 12", Kernel: "6.1.0-13-arm64",
		CPUModel: "Cortex-A72", CPUCores: 4, CPUThreads: 4, MemTotalBytes: 8 << 30,
		UptimeSec: 90061, // 1d 1h 1m
		LocalIP: "192.168.1.50", PublicIP: "203.0.113.7",
		Disks:     []core.HostDiskView{{Device: "nvme0n1", Model: "WD SN570", Rotational: false, SizeBytes: 512 << 30, FSType: "ext4", Mount: "/"}},
	}}
	var buf bytes.Buffer
	if code := run(api, []string{"host"}, &buf); code != 0 {
		t.Fatalf("run host exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"attic-pi", "Debian GNU/Linux 12", "Cortex-A72", "4 cores", "8.0 GiB", "1d 1h 1m", "nvme0n1", "WD SN570", "SSD", "192.168.1.50", "203.0.113.7"} {
		if !strings.Contains(out, want) {
			t.Errorf("host output missing %q\n%s", want, out)
		}
	}
}

func TestRunStatusShowsDegradedCollectors(t *testing.T) {
	api := &fakeAPI{snapshot: core.DashboardView{
		DegradedCollectors: []core.CollectorHealthView{
			{Name: "docker", Fails: 4, LastError: "Cannot connect to the Docker daemon"},
		},
	}}
	var buf bytes.Buffer
	if code := run(api, []string{"status"}, &buf); code != 0 {
		t.Fatalf("run status exit = %d, want 0", code)
	}
	out := buf.String()
	if !strings.Contains(out, "COLLECTOR DEGRADED") || !strings.Contains(out, "docker") || !strings.Contains(out, "Docker daemon") {
		t.Errorf("status did not surface the degraded collector:\n%s", out)
	}
}

func TestRunVersion(t *testing.T) {
	// ctl's own version is "dev" in a test binary (no -ldflags stamp); the core
	// version comes over the (fake) socket. Different values must flag a mismatch.
	api := &fakeAPI{version: "v9.9.9"}
	var buf bytes.Buffer
	if code := run(api, []string{"version"}, &buf); code != 0 {
		t.Fatalf("run version exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"v9.9.9", "ctl:", "core:", "different versions"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q:\n%s", want, out)
		}
	}
}

func TestRunLogs(t *testing.T) {
	api := &fakeAPI{containerLogs: "hello from web\nsecond line\n"}
	var buf bytes.Buffer
	if code := run(api, []string{"logs", "web", "--tail", "50"}, &buf); code != 0 {
		t.Fatalf("run logs exit = %d, want 0", code)
	}
	if got := buf.String(); !strings.Contains(got, "hello from web") || !strings.Contains(got, "second line") {
		t.Errorf("logs output = %q", got)
	}
	// Missing container name is a usage error (exit 2), not a crash.
	buf.Reset()
	if code := run(api, []string{"logs"}, &buf); code != 2 {
		t.Errorf("run logs (no container) exit = %d, want 2", code)
	}
}

func TestRunAlerts(t *testing.T) {
	api := &fakeAPI{active: []core.AlertRecord{
		{Key: "cpu", Severity: "crit", Source: "cpu>90", Time: 1700000000, Acked: true},
		{Key: "disk-root", Severity: "warn", Source: "disk /", Time: 1700000100},
	}}
	var buf bytes.Buffer
	if code := run(api, []string{"alerts"}, &buf); code != 0 {
		t.Fatalf("run alerts exit = %d, want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"cpu", "crit", "disk-root", "warn"} {
		if !strings.Contains(out, want) {
			t.Errorf("alerts output missing %q\n%s", want, out)
		}
	}
}

func TestRunAlertsEmpty(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, []string{"alerts"}, &buf); code != 0 {
		t.Fatalf("run alerts exit = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "no active alerts") {
		t.Errorf("empty alerts output = %q", buf.String())
	}
}

func TestRunUnknownSubcommand(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, []string{"bogus"}, &buf); code == 0 {
		t.Fatalf("run bogus exit = 0, want non-zero")
	}
}

func TestRunNoSubcommand(t *testing.T) {
	api := &fakeAPI{}
	var buf bytes.Buffer
	if code := run(api, nil, &buf); code == 0 {
		t.Fatalf("run with no args exit = 0, want non-zero")
	}
}
