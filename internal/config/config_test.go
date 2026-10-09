package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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
	if got, _ := c.Get("baseline_min_pct"); got != "0.15" {
		t.Fatalf("baseline_min_pct default = %q, want 0.15", got)
	}
	if got, _ := c.Get("fast_interval"); got != "5" {
		t.Fatalf("fast_interval default = %q, want 5", got)
	}
	if got, _ := c.Get("heartbeat_interval"); got != "30" {
		t.Fatalf("heartbeat_interval default = %q, want 30", got)
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

// TestBaselineMinPctRoundTrip confirms baseline_min_pct sets, gets, validates (>= 0), and
// unsets back to its 0.15 default.
func TestBaselineMinPctRoundTrip(t *testing.T) {
	c := Default()
	if err := c.Set("baseline_min_pct", "0.2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("baseline_min_pct"); got != "0.2" {
		t.Fatalf("after set = %q, want 0.2", got)
	}
	if err := c.Set("baseline_min_pct", "-0.1"); err == nil {
		t.Fatal("expected error for negative baseline_min_pct")
	}
	if err := c.Unset("baseline_min_pct"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("baseline_min_pct"); got != "0.15" {
		t.Fatalf("after unset = %q, want default 0.15", got)
	}
}

// TestBaselineAlertsDefaultOffRoundTrip pins Part 1 of the field-feedback fix:
// baseline-deviation alerting is opt-in, default OFF.
func TestBaselineAlertsDefaultOffRoundTrip(t *testing.T) {
	c := Default()
	if got, ok := c.Get("baseline_alerts"); !ok || got != "false" {
		t.Fatalf("baseline_alerts default = (%q, %v), want (false, true)", got, ok)
	}
	if err := c.Set("baseline_alerts", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("baseline_alerts"); got != "true" {
		t.Fatalf("after set true = %q, want true", got)
	}
	if err := c.Set("baseline_alerts", "not-a-bool"); err == nil {
		t.Fatal("expected error for non-bool baseline_alerts value")
	}
	if err := c.Unset("baseline_alerts"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("baseline_alerts"); got != "false" {
		t.Fatalf("after unset = %q, want default false", got)
	}
}

func TestBlockPrivateTargetsRoundTrip(t *testing.T) {
	c := Default()
	if got, ok := c.Get("notify.block_private_targets"); !ok || got != "false" {
		t.Fatalf("notify.block_private_targets default = (%q, %v), want (false, true)", got, ok)
	}
	if err := c.Set("notify.block_private_targets", "true"); err != nil {
		t.Fatal(err)
	}
	if !c.Notify.BlockPrivateTargets {
		t.Error("BlockPrivateTargets not set after Set true")
	}
	if got, _ := c.Get("notify.block_private_targets"); got != "true" {
		t.Errorf("after set = %q, want true", got)
	}
	if err := c.Set("notify.block_private_targets", "not-a-bool"); err == nil {
		t.Error("expected error for non-bool value")
	}
	if err := c.Unset("notify.block_private_targets"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("notify.block_private_targets"); got != "false" {
		t.Errorf("after unset = %q, want default false", got)
	}
}

func TestPublicIPToggleRoundTrip(t *testing.T) {
	c := Default()
	if c.PublicIPEnabled() {
		t.Error("collect.public_ip must default to false (opt-in)")
	}
	if got, _ := c.Get("collect.public_ip"); got != "false" {
		t.Errorf("default get = %q, want false", got)
	}
	if err := c.Set("collect.public_ip", "true"); err != nil {
		t.Fatal(err)
	}
	if !c.PublicIPEnabled() {
		t.Error("should be enabled after set true")
	}
	if err := c.Set("collect.public_ip", "not-a-bool"); err == nil {
		t.Error("want error for non-bool value")
	}
	if err := c.Unset("collect.public_ip"); err != nil {
		t.Fatal(err)
	}
	if c.PublicIPEnabled() {
		t.Error("should be back to disabled after unset")
	}
}

func TestServerNameRoundTripAndFallback(t *testing.T) {
	c := Default()
	// Unset: Get returns the resolved value, which falls back to the hostname.
	got, ok := c.Get("server.name")
	if !ok || got == "" {
		t.Fatalf("server.name default = (%q,%v), want a non-empty resolved name", got, ok)
	}
	if h, err := os.Hostname(); err == nil && h != "" && got != h {
		t.Errorf("server.name default = %q, want hostname %q", got, h)
	}
	// Set a custom name and read it back.
	if err := c.Set("server.name", "attic-pi"); err != nil {
		t.Fatal(err)
	}
	if c.ServerName() != "attic-pi" {
		t.Errorf("ServerName() = %q, want attic-pi", c.ServerName())
	}
	if got, _ := c.Get("server.name"); got != "attic-pi" {
		t.Errorf("Get after set = %q, want attic-pi", got)
	}
	// Empty clears the stored name back to the hostname fallback.
	if err := c.Set("server.name", ""); err != nil {
		t.Fatal(err)
	}
	if c.Name != "" {
		t.Errorf("Name after set empty = %q, want empty (falls back to hostname)", c.Name)
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

func TestTieredIntervalSetUnsetRoundTrip(t *testing.T) {
	c := Default()
	if err := c.Set("fast_interval", "10"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("fast_interval"); got != "10" {
		t.Fatalf("fast_interval after set = %q, want 10", got)
	}
	if err := c.Unset("fast_interval"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("fast_interval"); got != "5" {
		t.Fatalf("fast_interval after unset = %q, want default 5", got)
	}

	if err := c.Set("heartbeat_interval", "45"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("heartbeat_interval"); got != "45" {
		t.Fatalf("heartbeat_interval after set = %q, want 45", got)
	}
	if err := c.Unset("heartbeat_interval"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("heartbeat_interval"); got != "30" {
		t.Fatalf("heartbeat_interval after unset = %q, want default 30", got)
	}
}

func TestSampleIntervalMustBeMultipleOfFastInterval(t *testing.T) {
	c := Default()
	if err := c.Set("fast_interval", "5"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("sample_interval", "63"); err == nil {
		t.Fatal("expected error: 63 is not a multiple of fast_interval 5")
	}
	if err := c.Set("sample_interval", "60"); err != nil {
		t.Fatalf("60 is a multiple of 5, expected success: %v", err)
	}
	if got, _ := c.Get("sample_interval"); got != "60" {
		t.Fatalf("sample_interval = %q, want 60", got)
	}
}

func TestFastIntervalRejectedIfBreaksExistingMultiple(t *testing.T) {
	c := Default()
	if err := c.Set("sample_interval", "60"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("fast_interval", "7"); err == nil {
		t.Fatal("expected error: fast_interval 7 breaks sample_interval 60's multiple relationship")
	}
	// fast_interval must remain unchanged after the rejected Set.
	if got, _ := c.Get("fast_interval"); got != "5" {
		t.Fatalf("fast_interval = %q, want unchanged 5", got)
	}
	// A fast_interval that keeps the relationship intact still works.
	if err := c.Set("fast_interval", "10"); err != nil {
		t.Fatalf("fast_interval 10 divides sample_interval 60 evenly, expected success: %v", err)
	}
}

func TestFastIntervalCheckedAgainstEffectiveSampleIntervalOnBareConfig(t *testing.T) {
	// A directly-constructed &Config{} has SampleInterval==0, which Load() back-fills to 60.
	c := &Config{}
	if err := c.Set("fast_interval", "7"); err == nil {
		t.Fatal("expected error: effective sample_interval 60 is not a multiple of 7")
	}
	if c.FastInterval != 0 {
		t.Fatalf("FastInterval mutated to %d after rejected Set, want unchanged 0", c.FastInterval)
	}
	// A divisor of the effective 60 is accepted.
	if err := c.Set("fast_interval", "6"); err != nil {
		t.Fatalf("fast_interval 6 divides effective sample_interval 60, expected success: %v", err)
	}
}

func TestSampleIntervalConflictsWithPriorFastIntervalSet(t *testing.T) {
	// Symmetric case: set fast_interval first, then a conflicting
	// sample_interval Set must error.
	c := Default()
	if err := c.Set("fast_interval", "10"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("sample_interval", "55"); err == nil {
		t.Fatal("expected error: 55 is not a multiple of fast_interval 10")
	}
	if err := c.Set("sample_interval", "50"); err != nil {
		t.Fatalf("50 is a multiple of 10, expected success: %v", err)
	}
}

func TestTieredIntervalValidation(t *testing.T) {
	c := Default()
	if err := c.Set("fast_interval", "0"); err == nil {
		t.Error("expected error for fast_interval 0")
	}
	if err := c.Set("fast_interval", "abc"); err == nil {
		t.Error("expected error for non-integer fast_interval")
	}
	if err := c.Set("heartbeat_interval", "0"); err == nil {
		t.Error("expected error for heartbeat_interval 0")
	}
	if err := c.Set("heartbeat_interval", "abc"); err == nil {
		t.Error("expected error for non-integer heartbeat_interval")
	}
	// sample_interval min-5 rule must still hold.
	if err := c.Set("sample_interval", "4"); err == nil {
		t.Error("expected error for sample_interval below 5")
	}
}

func TestTieredIntervalsPersistAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("fast_interval", "10"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("heartbeat_interval", "20"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("sample_interval", "50"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("fast_interval"); got != "10" {
		t.Fatalf("fast_interval after load = %q, want 10", got)
	}
	if got, _ := c2.Get("heartbeat_interval"); got != "20" {
		t.Fatalf("heartbeat_interval after load = %q, want 20", got)
	}
	if got, _ := c2.Get("sample_interval"); got != "50" {
		t.Fatalf("sample_interval after load = %q, want 50", got)
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

func TestStorageBackendDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("storage.backend"); got != "tsfile" {
		t.Fatalf("storage.backend default = %q, want tsfile", got)
	}
	if err := c.Set("storage.backend", "memory"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.backend"); got != "memory" {
		t.Fatalf("storage.backend after set = %q, want memory", got)
	}
	if err := c.Set("storage.backend", "bogus"); err == nil {
		t.Fatal("storage.backend set to bogus: want error, got nil")
	}
	if got, _ := c.Get("storage.backend"); got != "memory" {
		t.Fatalf("storage.backend after rejected set = %q, want unchanged memory", got)
	}
	if err := c.Unset("storage.backend"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.backend"); got != "tsfile" {
		t.Fatalf("storage.backend after unset = %q, want default tsfile", got)
	}
}

func TestStorageBackendPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("storage.backend", "memory"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("storage.backend"); got != "memory" {
		t.Fatalf("storage.backend after save/load = %q, want memory", got)
	}
}

func TestStorageRetentionDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("storage.raw_retention"); got != "48h" {
		t.Fatalf("storage.raw_retention default = %q, want 48h", got)
	}
	if got, _ := c.Get("storage.rollup_retention"); got != "720h" {
		t.Fatalf("storage.rollup_retention default = %q, want 720h", got)
	}

	if err := c.Set("storage.raw_retention", "24h"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.raw_retention"); got != "24h" {
		t.Fatalf("storage.raw_retention after set = %q, want 24h", got)
	}
	if err := c.Set("storage.rollup_retention", "336h"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.rollup_retention"); got != "336h" {
		t.Fatalf("storage.rollup_retention after set = %q, want 336h", got)
	}

	if err := c.Unset("storage.raw_retention"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.raw_retention"); got != "48h" {
		t.Fatalf("storage.raw_retention after unset = %q, want default 48h", got)
	}
	if err := c.Unset("storage.rollup_retention"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.rollup_retention"); got != "720h" {
		t.Fatalf("storage.rollup_retention after unset = %q, want default 720h", got)
	}
}

func TestStorageRetentionRejectsBadDuration(t *testing.T) {
	c := Default()
	if err := c.Set("storage.raw_retention", "banana"); err == nil {
		t.Fatal("storage.raw_retention set to banana: want error, got nil")
	}
	if got, _ := c.Get("storage.raw_retention"); got != "48h" {
		t.Fatalf("storage.raw_retention after rejected set = %q, want unchanged 48h", got)
	}
	if err := c.Set("storage.rollup_retention", "banana"); err == nil {
		t.Fatal("storage.rollup_retention set to banana: want error, got nil")
	}
	if got, _ := c.Get("storage.rollup_retention"); got != "720h" {
		t.Fatalf("storage.rollup_retention after rejected set = %q, want unchanged 720h", got)
	}
	// Zero/negative durations are not useful retention windows either.
	if err := c.Set("storage.raw_retention", "0h"); err == nil {
		t.Fatal("storage.raw_retention set to 0h: want error, got nil")
	}
	if err := c.Set("storage.raw_retention", "-1h"); err == nil {
		t.Fatal("storage.raw_retention set to -1h: want error, got nil")
	}
}

func TestStorageRetentionPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("storage.raw_retention", "12h"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("storage.rollup_retention", "168h"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("storage.raw_retention"); got != "12h" {
		t.Fatalf("storage.raw_retention after save/load = %q, want 12h", got)
	}
	if got, _ := c2.Get("storage.rollup_retention"); got != "168h" {
		t.Fatalf("storage.rollup_retention after save/load = %q, want 168h", got)
	}
}

func TestContainerStatsDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("collect.container_stats"); got != "true" {
		t.Fatalf("collect.container_stats default = %q, want true", got)
	}
	if err := c.Set("collect.container_stats", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.container_stats"); got != "false" {
		t.Fatalf("collect.container_stats after set false = %q, want false", got)
	}
	if err := c.Set("collect.container_stats", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.container_stats"); got != "true" {
		t.Fatalf("collect.container_stats after set true = %q, want true", got)
	}
	if err := c.Set("collect.container_stats", "bogus"); err == nil {
		t.Fatal("collect.container_stats set to bogus: want error, got nil")
	}
	if err := c.Unset("collect.container_stats"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.container_stats"); got != "true" {
		t.Fatalf("collect.container_stats after unset = %q, want default true", got)
	}
}

func TestContainerStatsPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.container_stats", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.container_stats"); got != "false" {
		t.Fatalf("collect.container_stats after save/load = %q, want false (explicit false must persist)", got)
	}

	// And the reverse: an explicit true set after a prior false must also persist.
	if err := c2.Set("collect.container_stats", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c3.Get("collect.container_stats"); got != "true" {
		t.Fatalf("collect.container_stats after second save/load = %q, want true", got)
	}
}

func TestNetThroughputDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("collect.net_throughput"); got != "true" {
		t.Fatalf("collect.net_throughput default = %q, want true", got)
	}
	if err := c.Set("collect.net_throughput", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.net_throughput"); got != "false" {
		t.Fatalf("collect.net_throughput after set false = %q, want false", got)
	}
	if err := c.Set("collect.net_throughput", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.net_throughput"); got != "true" {
		t.Fatalf("collect.net_throughput after set true = %q, want true", got)
	}
	if err := c.Set("collect.net_throughput", "bogus"); err == nil {
		t.Fatal("collect.net_throughput set to bogus: want error, got nil")
	}
	if err := c.Unset("collect.net_throughput"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.net_throughput"); got != "true" {
		t.Fatalf("collect.net_throughput after unset = %q, want default true", got)
	}
}

func TestNetThroughputPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.net_throughput", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.net_throughput"); got != "false" {
		t.Fatalf("collect.net_throughput after save/load = %q, want false (explicit false must persist)", got)
	}

	if err := c2.Set("collect.net_throughput", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c3.Get("collect.net_throughput"); got != "true" {
		t.Fatalf("collect.net_throughput after second save/load = %q, want true", got)
	}
}

func TestServicesDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("collect.services"); got != "true" {
		t.Fatalf("collect.services default = %q, want true", got)
	}
	if err := c.Set("collect.services", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.services"); got != "false" {
		t.Fatalf("collect.services after set false = %q, want false", got)
	}
	if err := c.Set("collect.services", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.services"); got != "true" {
		t.Fatalf("collect.services after set true = %q, want true", got)
	}
	if err := c.Set("collect.services", "bogus"); err == nil {
		t.Fatal("collect.services set to bogus: want error, got nil")
	}
	if err := c.Unset("collect.services"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.services"); got != "true" {
		t.Fatalf("collect.services after unset = %q, want default true", got)
	}
}

func TestServicesPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.services", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.services"); got != "false" {
		t.Fatalf("collect.services after save/load = %q, want false (explicit false must persist)", got)
	}

	if err := c2.Set("collect.services", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c3.Get("collect.services"); got != "true" {
		t.Fatalf("collect.services after second save/load = %q, want true", got)
	}
}

func TestProcessesDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("collect.processes"); got != "true" {
		t.Fatalf("collect.processes default = %q, want true", got)
	}
	if err := c.Set("collect.processes", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.processes"); got != "false" {
		t.Fatalf("collect.processes after set false = %q, want false", got)
	}
	if err := c.Set("collect.processes", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.processes"); got != "true" {
		t.Fatalf("collect.processes after set true = %q, want true", got)
	}
	if err := c.Set("collect.processes", "bogus"); err == nil {
		t.Fatal("collect.processes set to bogus: want error, got nil")
	}
	if err := c.Unset("collect.processes"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.processes"); got != "true" {
		t.Fatalf("collect.processes after unset = %q, want default true", got)
	}
}

func TestProcessesPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.processes", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.processes"); got != "false" {
		t.Fatalf("collect.processes after save/load = %q, want false (explicit false must persist)", got)
	}

	if err := c2.Set("collect.processes", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c3.Get("collect.processes"); got != "true" {
		t.Fatalf("collect.processes after second save/load = %q, want true", got)
	}
}

func TestSmartAttrsDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("collect.smart_attrs"); got != "true" {
		t.Fatalf("collect.smart_attrs default = %q, want true", got)
	}
	if err := c.Set("collect.smart_attrs", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.smart_attrs"); got != "false" {
		t.Fatalf("collect.smart_attrs after set false = %q, want false", got)
	}
	if err := c.Set("collect.smart_attrs", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.smart_attrs"); got != "true" {
		t.Fatalf("collect.smart_attrs after set true = %q, want true", got)
	}
	if err := c.Set("collect.smart_attrs", "bogus"); err == nil {
		t.Fatal("collect.smart_attrs set to bogus: want error, got nil")
	}
	if err := c.Unset("collect.smart_attrs"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.smart_attrs"); got != "true" {
		t.Fatalf("collect.smart_attrs after unset = %q, want default true", got)
	}
}

func TestSmartAttrsPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.smart_attrs", "false"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.smart_attrs"); got != "false" {
		t.Fatalf("collect.smart_attrs after save/load = %q, want false (explicit false must persist)", got)
	}

	if err := c2.Set("collect.smart_attrs", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c2.Save(p); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c3.Get("collect.smart_attrs"); got != "true" {
		t.Fatalf("collect.smart_attrs after second save/load = %q, want true", got)
	}
}

func TestSmartIntervalSecDefault(t *testing.T) {
	c := Default()
	if got := c.SmartIntervalSec(); got != 1800 {
		t.Fatalf("SmartIntervalSec() default = %d, want 1800", got)
	}
	if got, _ := c.Get("collect.smart_interval"); got != "1800" {
		t.Fatalf("collect.smart_interval default = %q, want 1800", got)
	}
}

func TestSmartIntervalSetGetRoundTrip(t *testing.T) {
	c := Default()
	if err := c.Set("collect.smart_interval", "60"); err != nil {
		t.Fatal(err)
	}
	if got := c.SmartIntervalSec(); got != 60 {
		t.Fatalf("SmartIntervalSec() after set 60 = %d, want 60", got)
	}
	if got, _ := c.Get("collect.smart_interval"); got != "60" {
		t.Fatalf("collect.smart_interval after set 60 = %q, want 60", got)
	}
	if err := c.Unset("collect.smart_interval"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("collect.smart_interval"); got != "1800" {
		t.Fatalf("collect.smart_interval after unset = %q, want default 1800", got)
	}
}

func TestSmartIntervalSetRejectsInvalid(t *testing.T) {
	c := Default()
	for _, v := range []string{"0", "-5", "bogus"} {
		if err := c.Set("collect.smart_interval", v); err == nil {
			t.Errorf("collect.smart_interval set to %q: want error, got nil", v)
		}
	}
}

func TestEnrollBoundDefaults(t *testing.T) {
	c := Default()
	if got := c.EnrollMaxAttempts(); got != 5 {
		t.Errorf("EnrollMaxAttempts() default = %d, want 5", got)
	}
	if got := c.EnrollCooldownSec(); got != 60 {
		t.Errorf("EnrollCooldownSec() default = %d, want 60", got)
	}
	if got, _ := c.Get("telegram.enroll_max_attempts"); got != "5" {
		t.Errorf("telegram.enroll_max_attempts default = %q, want 5", got)
	}
	if got, _ := c.Get("telegram.enroll_cooldown"); got != "60" {
		t.Errorf("telegram.enroll_cooldown default = %q, want 60", got)
	}
}

func TestEnrollBoundSetGetUnsetRoundTrip(t *testing.T) {
	c := Default()
	if err := c.Set("telegram.enroll_max_attempts", "3"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("telegram.enroll_cooldown", "120"); err != nil {
		t.Fatal(err)
	}
	if got := c.EnrollMaxAttempts(); got != 3 {
		t.Errorf("EnrollMaxAttempts() after set = %d, want 3", got)
	}
	if got := c.EnrollCooldownSec(); got != 120 {
		t.Errorf("EnrollCooldownSec() after set = %d, want 120", got)
	}
	if err := c.Unset("telegram.enroll_max_attempts"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("telegram.enroll_max_attempts"); got != "5" {
		t.Errorf("telegram.enroll_max_attempts after unset = %q, want default 5", got)
	}
}

func TestEnrollBoundRejectsInvalid(t *testing.T) {
	c := Default()
	for _, key := range []string{"telegram.enroll_max_attempts", "telegram.enroll_cooldown"} {
		for _, v := range []string{"0", "-1", "bogus", ""} {
			if err := c.Set(key, v); err == nil {
				t.Errorf("%s set to %q: want error, got nil", key, v)
			}
		}
	}
}

func TestSmartIntervalPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("collect.smart_interval", "300"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("collect.smart_interval"); got != "300" {
		t.Fatalf("collect.smart_interval after save/load = %q, want 300", got)
	}
}

func TestWebEnabledListenDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("web.enabled"); got != "false" {
		t.Fatalf("web.enabled default = %q, want false", got)
	}
	if got, _ := c.Get("web.listen"); got != "127.0.0.1:8088" {
		t.Fatalf("web.listen default = %q, want 127.0.0.1:8088", got)
	}

	if err := c.Set("web.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.enabled"); got != "true" {
		t.Fatalf("web.enabled after set = %q, want true", got)
	}
	if err := c.Set("web.listen", "0.0.0.0:9090"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.listen"); got != "0.0.0.0:9090" {
		t.Fatalf("web.listen after set = %q, want 0.0.0.0:9090", got)
	}

	if err := c.Unset("web.enabled"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.enabled"); got != "false" {
		t.Fatalf("web.enabled after unset = %q, want default false", got)
	}
	if err := c.Unset("web.listen"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.listen"); got != "127.0.0.1:8088" {
		t.Fatalf("web.listen after unset = %q, want default 127.0.0.1:8088", got)
	}
}

func TestWebListenRejectsUnparsableHostPort(t *testing.T) {
	c := Default()
	if err := c.Set("web.listen", "not-a-host-port"); err == nil {
		t.Fatal("web.listen set to \"not-a-host-port\": want error, got nil")
	}
	if got, _ := c.Get("web.listen"); got != "127.0.0.1:8088" {
		t.Fatalf("web.listen after rejected set = %q, want unchanged 127.0.0.1:8088", got)
	}
}

func TestWebEnabledListenPersistsAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("web.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.listen", "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("web.enabled"); got != "true" {
		t.Fatalf("web.enabled after save/load = %q, want true", got)
	}
	if got, _ := c2.Get("web.listen"); got != "127.0.0.1:9999" {
		t.Fatalf("web.listen after save/load = %q, want 127.0.0.1:9999", got)
	}
}

func TestWebModeDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("web.mode"); got != "proxy" {
		t.Fatalf("web.mode default = %q, want proxy", got)
	}
	if err := c.Set("web.mode", "manual"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.mode"); got != "manual" {
		t.Fatalf("web.mode after set = %q, want manual", got)
	}
	if err := c.Unset("web.mode"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.mode"); got != "proxy" {
		t.Fatalf("web.mode after unset = %q, want default proxy", got)
	}
}

func TestWebModeRejectsInvalid(t *testing.T) {
	c := Default()
	for _, v := range []string{"", "https", "PROXY", "bogus"} {
		if err := c.Set("web.mode", v); err == nil {
			t.Errorf("web.mode set to %q: want error, got nil", v)
		}
	}
	if got, _ := c.Get("web.mode"); got != "proxy" {
		t.Fatalf("web.mode after rejected sets = %q, want unchanged proxy", got)
	}
	for _, v := range []string{"proxy", "autocert", "manual"} {
		if err := c.Set("web.mode", v); err != nil {
			t.Errorf("web.mode set to %q: want nil error, got %v", v, err)
		}
	}
}

func TestWebRPIDOriginAutocertTLSGetSetUnset(t *testing.T) {
	c := Default()
	for _, tc := range []struct{ key, want string }{
		{"web.rp_id", ""},
		{"web.origin", ""},
		{"web.autocert_domains", ""},
		{"web.tls_cert", ""},
		{"web.tls_key", ""},
	} {
		if got, ok := c.Get(tc.key); !ok || got != tc.want {
			t.Fatalf("%s default = %q (ok=%v), want %q", tc.key, got, ok, tc.want)
		}
	}

	sets := map[string]string{
		"web.rp_id":            "monitor.example.com",
		"web.origin":           "https://monitor.example.com",
		"web.autocert_domains": "monitor.example.com,alt.example.com",
		"web.tls_cert":         "/etc/trinetra/tls.crt",
		"web.tls_key":          "/etc/trinetra/tls.key",
	}
	for k, v := range sets {
		if err := c.Set(k, v); err != nil {
			t.Fatalf("Set(%q, %q): %v", k, v, err)
		}
		if got, _ := c.Get(k); got != v {
			t.Fatalf("%s after set = %q, want %q", k, got, v)
		}
	}

	for k := range sets {
		if err := c.Unset(k); err != nil {
			t.Fatalf("Unset(%q): %v", k, err)
		}
		if got, _ := c.Get(k); got != "" {
			t.Fatalf("%s after unset = %q, want empty default", k, got)
		}
	}
}

func TestWebSessionTTLDefaultSetUnsetRejectsInvalid(t *testing.T) {
	c := Default()
	if got, _ := c.Get("web.session_ttl"); got != "24h" {
		t.Fatalf("web.session_ttl default = %q, want 24h", got)
	}
	if err := c.Set("web.session_ttl", "1h"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.session_ttl"); got != "1h" {
		t.Fatalf("web.session_ttl after set = %q, want 1h", got)
	}
	for _, v := range []string{"", "not-a-duration", "0h", "-5m"} {
		if err := c.Set("web.session_ttl", v); err == nil {
			t.Errorf("web.session_ttl set to %q: want error, got nil", v)
		}
	}
	if got, _ := c.Get("web.session_ttl"); got != "1h" {
		t.Fatalf("web.session_ttl after rejected sets = %q, want unchanged 1h", got)
	}
	if err := c.Unset("web.session_ttl"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("web.session_ttl"); got != "24h" {
		t.Fatalf("web.session_ttl after unset = %q, want default 24h", got)
	}
}

func TestWebModeRPIDOriginPersistAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("web.mode", "manual"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.rp_id", "monitor.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.origin", "https://monitor.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.autocert_domains", "monitor.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.tls_cert", "/etc/trinetra/tls.crt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.tls_key", "/etc/trinetra/tls.key"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("web.session_ttl", "12h"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, want string }{
		{"web.mode", "manual"},
		{"web.rp_id", "monitor.example.com"},
		{"web.origin", "https://monitor.example.com"},
		{"web.autocert_domains", "monitor.example.com"},
		{"web.tls_cert", "/etc/trinetra/tls.crt"},
		{"web.tls_key", "/etc/trinetra/tls.key"},
		{"web.session_ttl", "12h"},
	} {
		if got, _ := c2.Get(tc.key); got != tc.want {
			t.Fatalf("%s after save/load = %q, want %q", tc.key, got, tc.want)
		}
	}
}

// TestPublicEnabledPanelsDefaultSetUnset pins public.enabled/public.panels (issue #67):
// both default to "off"/empty, Set/Get round-trip, and Unset restores the defaults.
func TestPublicEnabledPanelsDefaultSetUnset(t *testing.T) {
	c := Default()
	if got, _ := c.Get("public.enabled"); got != "false" {
		t.Fatalf("public.enabled default = %q, want false", got)
	}
	if got, _ := c.Get("public.panels"); got != "" {
		t.Fatalf("public.panels default = %q, want empty", got)
	}

	if err := c.Set("public.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("public.enabled"); got != "true" {
		t.Fatalf("public.enabled after set = %q, want true", got)
	}
	if err := c.Set("public.panels", "cpu,mem,disk:/"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("public.panels"); got != "cpu,mem,disk:/" {
		t.Fatalf("public.panels after set = %q, want cpu,mem,disk:/", got)
	}

	if err := c.Unset("public.enabled"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("public.enabled"); got != "false" {
		t.Fatalf("public.enabled after unset = %q, want default false", got)
	}
	if err := c.Unset("public.panels"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("public.panels"); got != "" {
		t.Fatalf("public.panels after unset = %q, want default empty", got)
	}
}

// TestPublicPanelsRejectsUnknownPanel pins the server-side allowlist at the config layer:
// only the fixed catalog (or "disk:<mount>") may be stored in public.panels.
func TestPublicPanelsRejectsUnknownPanel(t *testing.T) {
	c := Default()
	for _, v := range []string{"bogus", "cpu,bogus", "disk:", "users", "config", "channels"} {
		if err := c.Set("public.panels", v); err == nil {
			t.Errorf("public.panels set to %q: want error, got nil", v)
		}
	}
	if got, _ := c.Get("public.panels"); got != "" {
		t.Fatalf("public.panels after rejected sets = %q, want unchanged empty", got)
	}
	for _, v := range []string{"availability", "cpu", "mem", "swap", "load", "temp", "uptime", "services", "containers", "net", "disk:/", "disk:/data"} {
		if err := c.Set("public.panels", v); err != nil {
			t.Errorf("public.panels set to %q: want nil error, got %v", v, err)
		}
	}
}

// TestPublicEnabledPanelsPersistAcrossSaveLoad pins the Save/Load round trip
// for both keys together, mirroring TestWebEnabledListenPersistsAcrossSaveLoad.
func TestPublicEnabledPanelsPersistAcrossSaveLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	c := Default()
	if err := c.Set("public.enabled", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("public.panels", "cpu,mem,disk:/data,uptime"); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c2.Get("public.enabled"); got != "true" {
		t.Fatalf("public.enabled after save/load = %q, want true", got)
	}
	if got, _ := c2.Get("public.panels"); got != "cpu,mem,disk:/data,uptime" {
		t.Fatalf("public.panels after save/load = %q, want cpu,mem,disk:/data,uptime", got)
	}
}

func TestLoadBackfillsMissingRetentionKeys(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	// Simulate a config.json written before storage retention keys existed.
	if err := os.WriteFile(p, []byte(`{"storage":{"backend":"tsfile"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get("storage.raw_retention"); got != "48h" {
		t.Fatalf("backfilled storage.raw_retention = %q, want 48h", got)
	}
	if got, _ := c.Get("storage.rollup_retention"); got != "720h" {
		t.Fatalf("backfilled storage.rollup_retention = %q, want 720h", got)
	}
}

// --- Task 2 (#91): config key catalog (KeyInfo/Keys) ---

// TestKeyCatalogMatchesSetAndGet asserts every catalog entry names a real Set/Get key.
func TestKeyCatalogMatchesSetAndGet(t *testing.T) {
	for _, ki := range Keys() {
		c := Default()
		val, ok := c.Get(ki.Name)
		if !ok {
			t.Errorf("Keys() entry %q: c.Get did not recognize it as a key", ki.Name)
			continue
		}
		if err := c.Set(ki.Name, val); err != nil {
			t.Errorf("Keys() entry %q: Set(Get()) round trip on Default() failed: %v", ki.Name, err)
		}
		if ki.Group == "" {
			t.Errorf("Keys() entry %q: missing Group", ki.Name)
		}
		if ki.Help == "" {
			t.Errorf("Keys() entry %q: missing Help", ki.Name)
		}
		switch ki.Kind {
		case "int":
			if _, err := strconv.Atoi(val); err != nil {
				t.Errorf("Keys() entry %q: Kind=int but Get() value %q does not parse as int: %v", ki.Name, val, err)
			}
		case "float":
			if _, err := strconv.ParseFloat(val, 64); err != nil {
				t.Errorf("Keys() entry %q: Kind=float but Get() value %q does not parse as float: %v", ki.Name, val, err)
			}
		case "bool":
			if _, err := strconv.ParseBool(val); err != nil {
				t.Errorf("Keys() entry %q: Kind=bool but Get() value %q does not parse as bool: %v", ki.Name, val, err)
			}
		case "duration":
			if val != "" {
				if _, err := time.ParseDuration(val); err != nil {
					t.Errorf("Keys() entry %q: Kind=duration but Get() value %q does not parse: %v", ki.Name, val, err)
				}
			}
		case "string", "enum", "csv":
			// No format constraint beyond the Set/Get round trip above.
		default:
			t.Errorf("Keys() entry %q: unknown Kind %q", ki.Name, ki.Kind)
		}
	}
}

// setSwitchKeys parses config.go's own source and extracts every string case label in
// (*Config).Set's switch statement.
func setSwitchKeys(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing config.go: %v", err)
	}
	var keys []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Set" || fn.Recv == nil || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if s, err := strconv.Unquote(lit.Value); err == nil {
					keys = append(keys, s)
				}
			}
			return true
		})
		return false // don't descend into other funcs
	})
	return keys
}

// TestKeyCatalogCoversEverySetKey asserts the catalog's key names are EXACTLY the set of
// keys (*Config).Set accepts: no key Set handles is missing from the catalog.
func TestKeyCatalogCoversEverySetKey(t *testing.T) {
	setKeys := setSwitchKeys(t)
	if len(setKeys) == 0 {
		t.Fatal("setSwitchKeys found no case labels -- test helper is broken")
	}

	catalog := map[string]bool{}
	for _, ki := range Keys() {
		if catalog[ki.Name] {
			t.Errorf("Keys() lists %q more than once", ki.Name)
		}
		catalog[ki.Name] = true
	}

	seen := map[string]bool{}
	for _, k := range setKeys {
		seen[k] = true
		if !catalog[k] {
			t.Errorf("Set accepts %q but Keys() does not list it (catalog is missing a key)", k)
		}
	}
	for name := range catalog {
		if !seen[name] {
			t.Errorf("Keys() lists %q but Set has no case for it (stale catalog entry)", name)
		}
	}
}

// The master reads fleet.node_down_after once, when it builds its liveness
// tracker at start, so the key needs a restart like the other fleet tunables.
func TestFleetKeysRequireRestart(t *testing.T) {
	want := map[string]bool{"fleet.listen": true, "fleet.outbox_max_mb": true, "fleet.node_down_after": true}
	for _, k := range Keys() {
		if want[k.Name] {
			if !k.RestartRequired {
				t.Errorf("%s: RestartRequired = false", k.Name)
			}
			delete(want, k.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("keys missing from catalog: %v", want)
	}
}

// fleet.fallback_after and fleet.link_down_warn_after apply live.
func TestFleetFallbackAndLinkDownWarnKeysApplyLive(t *testing.T) {
	dontWant := map[string]bool{"fleet.fallback_after": true, "fleet.link_down_warn_after": true}
	for _, k := range Keys() {
		if dontWant[k.Name] {
			if k.RestartRequired {
				t.Errorf("%s: RestartRequired = true, want false (applies live)", k.Name)
			}
			delete(dontWant, k.Name)
		}
	}
	if len(dontWant) != 0 {
		t.Fatalf("keys missing from catalog: %v", dontWant)
	}
}

func TestFleetFallbackAfterDefaultAndSet(t *testing.T) {
	c := &Config{}
	if got := c.FleetFallbackAfter(); got != 2*time.Minute {
		t.Fatalf("default = %s, want 2m", got)
	}
	if err := c.Set("fleet.fallback_after", "90s"); err != nil {
		t.Fatal(err)
	}
	if got := c.FleetFallbackAfter(); got != 90*time.Second {
		t.Fatalf("after set = %s, want 90s", got)
	}
	if got, ok := c.Get("fleet.fallback_after"); !ok || got != "1m30s" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "not-a-duration", "1s", "-1m"} {
		if err := c.Set("fleet.fallback_after", bad); err == nil {
			t.Errorf("Set(%q) accepted, want a validation error", bad)
		}
	}
	// A rejected Set must not have clobbered the last good value.
	if got := c.FleetFallbackAfter(); got != 90*time.Second {
		t.Fatalf("after rejected sets = %s, want unchanged 90s", got)
	}
}

func TestFleetLinkDownWarnAfterDefaultAndSet(t *testing.T) {
	c := &Config{}
	if got := c.FleetLinkDownWarnAfter(); got != 10*time.Minute {
		t.Fatalf("default = %s, want 10m", got)
	}
	if err := c.Set("fleet.link_down_warn_after", "5m"); err != nil {
		t.Fatal(err)
	}
	if got := c.FleetLinkDownWarnAfter(); got != 5*time.Minute {
		t.Fatalf("after set = %s, want 5m", got)
	}
	if got, ok := c.Get("fleet.link_down_warn_after"); !ok || got != "5m0s" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	for _, bad := range []string{"", "not-a-duration", "1s", "-1m"} {
		if err := c.Set("fleet.link_down_warn_after", bad); err == nil {
			t.Errorf("Set(%q) accepted, want a validation error", bad)
		}
	}
}

func TestStatusKeys(t *testing.T) {
	c := Default()
	if c.StatusTitle() != "Status" || c.StatusAutoResolveAfter() != 24*time.Hour {
		t.Fatalf("defaults: %q %v", c.StatusTitle(), c.StatusAutoResolveAfter())
	}
	for k, v := range map[string]string{"status.title": "Acme status", "status.auto_resolve_after": "0", "status.echo_channels": "tg, ops-email"} {
		if err := c.Set(k, v); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	if c.StatusAutoResolveAfter() != 0 {
		t.Fatal("0 must mean never")
	}
	if got, _ := c.Get("status.echo_channels"); got != "tg,ops-email" {
		t.Fatalf("echo_channels %q", got)
	}
	for _, kv := range [][2]string{{"status.title", strings.Repeat("x", 61)}, {"status.auto_resolve_after", "30s"}, {"status.auto_resolve_after", "-1h"}} {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Errorf("Set(%s,%q) accepted", kv[0], kv[1])
		}
	}
	for _, k := range []string{"status.title", "status.auto_resolve_after", "status.echo_channels"} {
		if err := c.Unset(k); err != nil {
			t.Errorf("Unset(%s): %v", k, err)
		}
	}
}

func TestSetupCompletedKey(t *testing.T) {
	c := Default()
	if got, _ := c.Get("setup.completed"); got != "false" {
		t.Fatalf("default = %q", got)
	}
	if err := c.Set("setup.completed", "true"); err != nil {
		t.Fatal(err)
	}
	if !c.Setup.Completed {
		t.Fatal("not set")
	}
	if err := c.Unset("setup.completed"); err != nil || c.Setup.Completed {
		t.Fatalf("unset: %v %v", err, c.Setup.Completed)
	}
	found := false
	for _, k := range Keys() {
		found = found || k.Name == "setup.completed"
	}
	if !found {
		t.Fatal("missing from Keys()")
	}
}
