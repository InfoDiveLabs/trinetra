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
	// A directly-constructed &Config{} has SampleInterval==0, which Load()
	// back-fills to 60. Set("fast_interval","7") must reject rather than
	// succeed silently and let Load() persist an inconsistent 60%7!=0 config.
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
