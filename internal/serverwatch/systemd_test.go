package serverwatch

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
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
