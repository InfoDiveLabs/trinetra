package serverwatch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyFileAtomicReplace guards the rename-based copyFile: it must replace
// an existing dst (the running-binary upgrade path) with the new content and
// perm, and leave no ".tmp-install" scratch behind. rename(2) — not a
// truncating write — is what makes this ETXTBSY-safe for a live daemon.
func TestCopyFileAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("NEW-BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "NEW-BINARY" {
		t.Errorf("dst content = %q, want NEW-BINARY", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("dst perm = %v, want 0755", fi.Mode().Perm())
	}
	if _, err := os.Stat(dst + ".tmp-install"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind: %v", err)
	}
}

func TestRenderUnit(t *testing.T) {
	u := renderUnit("/usr/local/bin/serverwatch")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]",
		"ExecStart=/usr/local/bin/serverwatch daemon",
		"Restart=always",
		"WatchdogSec=",
		"RuntimeDirectory=serverwatch",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit missing %q:\n%s", want, u)
		}
	}
}

// TestCmdDoctorPrintsCollectorSummary is a CLI-level smoke test for
// cmdDoctor: it loads the configured store (via openConfiguredStore, same
// helper migrate/dump use) and renders the collector on/off toggles plus
// SampleStore stats via buildDoctorReport/renderDoctorReport (systemd.go).
// Exercises the real tsfile-backend path end to end, not just those helpers
// in isolation.
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
