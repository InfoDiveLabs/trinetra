package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// fakeAPI is a core.API stub returning canned values so run() can be driven
// without a real control socket. Only the reads the ctl subcommands exercise
// are populated; the rest satisfy the interface and are never called by the
// tested paths.
type fakeAPI struct {
	snapshot core.DashboardView
	doctor   core.DoctorReport
	active   []core.AlertRecord
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
func (f *fakeAPI) Config() (*config.Config, error)    { return &config.Config{}, nil }
func (f *fakeAPI) Doctor() (core.DoctorReport, error) { return f.doctor, nil }
func (f *fakeAPI) ApplyConfig(*config.Config) error   { return nil }
func (f *fakeAPI) AckAlert(key string) error          { return nil }
func (f *fakeAPI) UnackAlert(key string) error        { return nil }
func (f *fakeAPI) TestChannel(name string) error      { return nil }
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
