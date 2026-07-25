package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndGet(t *testing.T) {
	c := Default()
	if got, _ := c.Get("sample_interval"); got != "60" {
		t.Fatalf("sample_interval default = %q, want 60", got)
	}
	if got, _ := c.Get("thresholds.disk_pct"); got != "90" {
		t.Fatalf("disk_pct default = %q, want 90", got)
	}
	if got, _ := c.Get("baseline_sigma"); got != "3" {
		t.Fatalf("baseline_sigma default = %q, want 3", got)
	}
}

func TestSetUnsetRoundTrip(t *testing.T) {
	c := Default()
	if err := c.Set("sample_interval", "30"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("sample_interval"); got != "30" {
		t.Fatalf("after set = %q, want 30", got)
	}
	if err := c.Unset("sample_interval"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("sample_interval"); got != "60" {
		t.Fatalf("after unset = %q, want default 60", got)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "config.json")
	c := Default()
	_ = c.Set("telegram.token", "abc")
	_ = c.Set("thresholds.disk_pct", "80")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("telegram.token"); got != "abc" {
		t.Fatalf("token = %q", got)
	}
	if got, _ := c2.Get("thresholds.disk_pct"); got != "80" {
		t.Fatalf("disk_pct = %q", got)
	}
}

func TestSaveTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// Pre-create the file with looser 0644 perms.
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

func TestLoadMissingIsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if got, _ := c.Get("sample_interval"); got != "60" {
		t.Fatalf("want defaults, got %q", got)
	}
}

func TestTargetOverrides(t *testing.T) {
	c := Default()
	if !c.TargetEnabled("docker:foo") {
		t.Fatal("targets enabled by default")
	}
	c.SetTarget("docker:foo", false)
	if c.TargetEnabled("docker:foo") {
		t.Fatal("should be disabled")
	}
	c.SetTargetThreshold("disk:/boot", 70)
	if v, ok := c.TargetThreshold("disk:/boot"); !ok || v != 70 {
		t.Fatalf("threshold = %v ok=%v", v, ok)
	}
}
