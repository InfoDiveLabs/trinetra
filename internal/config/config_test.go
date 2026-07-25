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

func TestSaveAtomicOverwriteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// First save.
	c := Default()
	_ = c.Set("telegram.token", "first")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	// Overwrite atomically with a new value; the .tmp must not linger.
	_ = c.Set("telegram.token", "second")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should not remain after Save: err=%v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("telegram.token"); got != "second" {
		t.Fatalf("token after overwrite = %q, want second", got)
	}
}

func TestSetScheduleAndQuietHoursValidation(t *testing.T) {
	valid := []struct{ key, val string }{
		{"schedule.daily", ""},
		{"schedule.daily", "09:00"},
		{"schedule.daily", "23:59"},
		{"schedule.weekly", ""},
		{"schedule.weekly", "mon@09:00"},
		{"schedule.weekly", "SUN@00:00"},
		{"quiet_hours", ""},
		{"quiet_hours", "23-8"},
		{"quiet_hours", "0-23"},
	}
	for _, tc := range valid {
		c := Default()
		if err := c.Set(tc.key, tc.val); err != nil {
			t.Errorf("Set(%q,%q) unexpected error: %v", tc.key, tc.val, err)
		}
	}
	invalid := []struct{ key, val string }{
		{"schedule.daily", "9am"},
		{"schedule.daily", "24:00"},
		{"schedule.daily", "09:60"},
		{"schedule.weekly", "funday@09:00"},
		{"schedule.weekly", "mon-09:00"},
		{"schedule.weekly", "mon@25:00"},
		{"quiet_hours", "23"},
		{"quiet_hours", "23-24"},
		{"quiet_hours", "a-b"},
	}
	for _, tc := range invalid {
		c := Default()
		if err := c.Set(tc.key, tc.val); err == nil {
			t.Errorf("Set(%q,%q) expected error, got nil", tc.key, tc.val)
		}
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

func TestChannelAddGetRemoveRoundTrip(t *testing.T) {
	c := Default()
	c.AddChannel(ChannelConfig{Name: "tg", Type: "telegram", Enabled: true, MinSeverity: "info"})
	got, ok := c.GetChannel("tg")
	if !ok || got.Type != "telegram" {
		t.Fatalf("GetChannel = %+v, ok=%v", got, ok)
	}
	if _, ok := c.GetChannel("nope"); ok {
		t.Fatal("expected unknown channel to report not found")
	}
	if !c.RemoveChannel("tg") {
		t.Fatal("expected RemoveChannel to report found")
	}
	if _, ok := c.GetChannel("tg"); ok {
		t.Fatal("expected channel gone after remove")
	}
	if c.RemoveChannel("tg") {
		t.Fatal("expected second RemoveChannel of same name to report false")
	}
}

func TestSetChannelField(t *testing.T) {
	c := Default()
	c.AddChannel(ChannelConfig{Name: "tg", Type: "telegram", Enabled: true, MinSeverity: "info"})

	if err := c.SetChannelField("tg", "enabled", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "min_severity", "critical"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "include_kinds", "disk,cpu"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "exclude_kinds", "smart"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "critical_overrides_quiet", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "setting.chat_id", "12345"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannelField("tg", "type", "webhook"); err != nil {
		t.Fatal(err)
	}

	got, _ := c.GetChannel("tg")
	if got.Enabled {
		t.Error("enabled should be false")
	}
	if got.Type != "webhook" {
		t.Errorf("type = %q, want webhook", got.Type)
	}
	if got.MinSeverity != "critical" {
		t.Errorf("min_severity = %q", got.MinSeverity)
	}
	if len(got.IncludeKinds) != 2 || got.IncludeKinds[0] != "disk" || got.IncludeKinds[1] != "cpu" {
		t.Errorf("include_kinds = %v", got.IncludeKinds)
	}
	if len(got.ExcludeKinds) != 1 || got.ExcludeKinds[0] != "smart" {
		t.Errorf("exclude_kinds = %v", got.ExcludeKinds)
	}
	if !got.CriticalOverridesQuiet {
		t.Error("critical_overrides_quiet should be true")
	}
	if got.Settings["chat_id"] != "12345" {
		t.Errorf("settings.chat_id = %q", got.Settings["chat_id"])
	}

	if err := c.SetChannelField("tg", "min_severity", "bogus"); err == nil {
		t.Error("expected error for invalid min_severity")
	}
	if err := c.SetChannelField("nope", "enabled", "true"); err == nil {
		t.Error("expected error for unknown channel")
	}
	if err := c.SetChannelField("tg", "unknown_key", "x"); err == nil {
		t.Error("expected error for unknown field")
	}
	if err := c.SetChannelField("tg", "enabled", "not-a-bool"); err == nil {
		t.Error("expected error for invalid bool")
	}
}

func TestChannelsPersistAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	c.AddChannel(ChannelConfig{
		Name: "tg", Type: "telegram", Enabled: true, MinSeverity: "warning",
		Settings: map[string]string{"chat_id": "1"},
	})
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c2.GetChannel("tg")
	if !ok {
		t.Fatal("channel missing after load")
	}
	if got.MinSeverity != "warning" || got.Settings["chat_id"] != "1" {
		t.Errorf("channel after load = %+v", got)
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
