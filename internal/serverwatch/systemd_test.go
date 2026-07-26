package serverwatch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

func TestRenderUnit(t *testing.T) {
	u := renderUnit("/usr/local/bin/serverwatch")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"ExecStart=/usr/local/bin/serverwatch daemon",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit missing %q:\n%s", want, u)
		}
	}
}

// TestCollectorSummaryListsTogglesAndStoreStats asserts collectorSummary
// (the testable helper behind `serverwatch doctor`'s cardinality/disk
// guardrail output, docs/ROADMAP.md Epic #69 x7) reports every collect.*
// toggle's on/off state plus the SampleStore's series count and disk
// footprint.
func TestCollectorSummaryListsTogglesAndStoreStats(t *testing.T) {
	c := config.Default()
	if err := c.Set("collect.services", "false"); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Append(1, MetricSet{"cpu": 1, "mem": 2}); err != nil {
		t.Fatal(err)
	}

	got := collectorSummary(c, store)
	for _, want := range []string{
		"container_stats=on", "net_throughput=on", "services=off",
		"processes=on", "smart_attrs=on",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("collectorSummary missing %q; got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "2 series") {
		t.Errorf("collectorSummary missing series count; got:\n%s", got)
	}
}

// TestCollectorSummaryNilStoreUnavailable asserts a nil store (the
// configured backend failed to open) renders "unavailable" rather than
// panicking.
func TestCollectorSummaryNilStoreUnavailable(t *testing.T) {
	c := config.Default()
	got := collectorSummary(c, nil)
	if !strings.Contains(got, "unavailable") {
		t.Errorf("collectorSummary(nil store) = %q, want it to mention unavailable", got)
	}
}

// TestCmdDoctorPrintsCollectorSummary is a CLI-level smoke test for the
// wiring added in cmdDoctor: it loads the configured store (via
// openConfiguredStore, same helper migrate/dump use) and appends
// collectorSummary's output. Exercises the real tsfile-backend path end to
// end (not just the collectorSummary helper in isolation).
func TestCmdDoctorPrintsCollectorSummary(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	stateDir = filepath.Join(dir, "state")

	var out bytes.Buffer
	stdout = &out
	if code := Main([]string{"doctor"}); code != 0 {
		t.Fatalf("doctor exit=%d", code)
	}
	got := out.String()
	for _, want := range []string{
		"collectors: container_stats=on net_throughput=on services=on processes=on smart_attrs=on",
		"time-series:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output missing %q; got:\n%s", want, got)
		}
	}
}

func TestQuietHoursAndScheduleRequireArgs(t *testing.T) {
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")
	var out, errb bytes.Buffer
	stdout = &out
	stderr = &errb

	cases := []struct {
		name string
		args []string
	}{
		{"quiet-hours no arg", []string{"quiet-hours"}},
		{"quiet-hours too many", []string{"quiet-hours", "23-8", "extra"}},
		{"schedule no arg", []string{"schedule"}},
		{"schedule daily no value", []string{"schedule", "daily"}},
		{"schedule weekly no value", []string{"schedule", "weekly"}},
	}
	for _, tc := range cases {
		errb.Reset()
		if code := Main(tc.args); code != 2 {
			t.Fatalf("%s: exit=%d, want 2 (stderr=%q)", tc.name, code, errb.String())
		}
	}
}
