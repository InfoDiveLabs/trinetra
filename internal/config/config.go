// Package config is the CLI-managed configuration store for serverwatch.
// There is no env or hand-edited file: all keys are set via `serverwatch config set`.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config is persisted as JSON. Zero values mean "use default"; Get resolves
// the effective value by falling back to Default() for unset scalar keys.
type Config struct {
	// SampleInterval is the slow tier: seconds between full baseline samples.
	SampleInterval int `json:"sample_interval,omitempty"`
	// FastInterval is the fast tier: seconds between lightweight checks.
	FastInterval int `json:"fast_interval,omitempty"`
	// HeartbeatInterval is the seconds between liveness heartbeats.
	HeartbeatInterval int     `json:"heartbeat_interval,omitempty"`
	BaselineSigma     float64 `json:"baseline_sigma,omitempty"`
	QuietHours        string  `json:"quiet_hours,omitempty"` // "23-8" or ""
	Telegram          struct {
		Token  string `json:"token,omitempty"`
		ChatID string `json:"chat_id,omitempty"`
	} `json:"telegram"`
	Healthchecks struct {
		URL string `json:"url,omitempty"`
	} `json:"healthchecks"`
	Schedule struct {
		Daily  string `json:"daily,omitempty"`  // "09:00" or ""
		Weekly string `json:"weekly,omitempty"` // "mon@09:00" or ""
	} `json:"schedule"`
	Thresholds struct {
		DiskPct float64 `json:"disk_pct,omitempty"`
		TempC   float64 `json:"temp_c,omitempty"`
		CPUPct  float64 `json:"cpu_pct,omitempty"`
		MemPct  float64 `json:"mem_pct,omitempty"`
		SwapPct float64 `json:"swap_pct,omitempty"`
	} `json:"thresholds"`
	// Targets holds per-target overrides keyed by namespaced target id.
	Targets map[string]TargetOverride `json:"targets,omitempty"`
	// CriticalOverridesQuiet lets disk-full-imminent style alerts bypass quiet hours.
	CriticalOverridesQuiet bool `json:"critical_overrides_quiet,omitempty"`
	// Channels holds user-defined notification channels, managed via
	// `serverwatch channel add|list|remove|set|test`.
	Channels []ChannelConfig `json:"channels,omitempty"`
	Storage  struct {
		// Backend selects the SampleStore implementation (see
		// internal/serverwatch/samplestore.go and docs/DESIGN-storage.md).
		// One of validStorageBackends; defaults to "tsfile".
		Backend string `json:"backend,omitempty"`
		// RawRetention/RollupRetention are duration strings (time.ParseDuration
		// syntax, e.g. "48h") controlling how long the tsfile backend keeps raw
		// and 1m-rollup samples respectively (see docs/DESIGN-storage.md). The
		// event retention window reuses RollupRetention. Defaults: 48h / 720h.
		RawRetention    string `json:"raw_retention,omitempty"`
		RollupRetention string `json:"rollup_retention,omitempty"`
	} `json:"storage"`
}

type TargetOverride struct {
	Disabled  bool     `json:"disabled,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// ChannelConfig describes one user-configured notification channel. The
// concrete delivery mechanism (Telegram, email, webhook, ...) is chosen by
// Type and is built elsewhere (package serverwatch's buildNotifier factory);
// this package only stores and validates the configuration.
type ChannelConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// Settings holds type-specific key/value config, e.g. {"chat_id": "..."}.
	Settings map[string]string `json:"settings,omitempty"`
	// MinSeverity is "info" | "warning" | "critical". Empty is treated as the
	// permissive default ("info") wherever routing is evaluated.
	MinSeverity            string   `json:"min_severity,omitempty"`
	IncludeKinds           []string `json:"include_kinds,omitempty"`
	ExcludeKinds           []string `json:"exclude_kinds,omitempty"`
	CriticalOverridesQuiet bool     `json:"critical_overrides_quiet,omitempty"`
}

// validSeverities is a local allowlist mirroring serverwatch.Severity's
// string form. config cannot import package serverwatch (that would create
// an import cycle, since serverwatch imports config), so severity strings
// are validated here independently rather than via serverwatch.ParseSeverity.
var validSeverities = map[string]bool{"info": true, "warning": true, "critical": true}

// validStorageBackends allowlists storage.backend. "tsfile" is the design's
// default backend (docs/DESIGN-storage.md, lands in a later task); "memory"
// is the in-memory reference SampleStore (internal/serverwatch/samplestore.go).
var validStorageBackends = map[string]bool{"tsfile": true, "memory": true}

// validateStorageBackend rejects anything outside validStorageBackends.
func validateStorageBackend(s string) error {
	if validStorageBackends[s] {
		return nil
	}
	return fmt.Errorf("storage.backend %q invalid: want one of tsfile|memory", s)
}

// validateRetentionDuration rejects anything time.ParseDuration can't parse,
// plus non-positive durations (a zero or negative retention window is never
// a useful policy: it would mean "keep nothing").
func validateRetentionDuration(key, s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%s %q invalid: %w (want a duration like 48h)", key, s, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s %q invalid: must be positive", key, s)
	}
	return nil
}

// validateMinSeverity accepts "" (meaning "use the permissive default") or
// one of info|warning|critical.
func validateMinSeverity(s string) error {
	if s == "" || validSeverities[s] {
		return nil
	}
	return fmt.Errorf("min_severity %q invalid: want info|warning|critical or empty", s)
}

// AddChannel appends a new channel. Callers are responsible for checking
// for an existing channel of the same name first, if that matters to them.
func (c *Config) AddChannel(cc ChannelConfig) {
	c.Channels = append(c.Channels, cc)
}

// RemoveChannel deletes the channel named name, reporting whether one was
// found.
func (c *Config) RemoveChannel(name string) bool {
	for i, cc := range c.Channels {
		if cc.Name == name {
			c.Channels = append(c.Channels[:i], c.Channels[i+1:]...)
			return true
		}
	}
	return false
}

// GetChannel returns a pointer to the named channel's config (so callers
// such as SetChannelField can mutate it in place), or (nil, false) if no
// channel by that name exists.
func (c *Config) GetChannel(name string) (*ChannelConfig, bool) {
	for i := range c.Channels {
		if c.Channels[i].Name == name {
			return &c.Channels[i], true
		}
	}
	return nil, false
}

// splitKinds parses a comma-separated kind list, trimming whitespace and
// dropping empty elements. An empty string yields a nil slice (clears the
// field).
func splitKinds(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// SetChannelField updates one field of an existing channel by name. Valid
// keys: enabled, type, min_severity, critical_overrides_quiet,
// include_kinds, exclude_kinds (comma-separated), and setting.<k> which
// writes into the channel's Settings map.
func (c *Config) SetChannelField(name, key, value string) error {
	cc, ok := c.GetChannel(name)
	if !ok {
		return fmt.Errorf("unknown channel %q", name)
	}
	switch {
	case key == "enabled":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("enabled: %w", err)
		}
		cc.Enabled = b
	case key == "type":
		cc.Type = value
	case key == "min_severity":
		if err := validateMinSeverity(value); err != nil {
			return err
		}
		cc.MinSeverity = value
	case key == "critical_overrides_quiet":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("critical_overrides_quiet: %w", err)
		}
		cc.CriticalOverridesQuiet = b
	case key == "include_kinds":
		cc.IncludeKinds = splitKinds(value)
	case key == "exclude_kinds":
		cc.ExcludeKinds = splitKinds(value)
	case strings.HasPrefix(key, "setting."):
		k := strings.TrimPrefix(key, "setting.")
		if k == "" {
			return fmt.Errorf("empty setting key")
		}
		if cc.Settings == nil {
			cc.Settings = map[string]string{}
		}
		cc.Settings[k] = value
	default:
		return fmt.Errorf("unknown channel field %q", key)
	}
	return nil
}

// Default returns the baked-in defaults. A fresh install works with only a token.
func Default() *Config {
	c := &Config{
		SampleInterval:    60,
		FastInterval:      5,
		HeartbeatInterval: 30,
		BaselineSigma:     3,
	}
	c.Thresholds.DiskPct = 90
	c.Thresholds.TempC = 80
	c.Thresholds.CPUPct = 95
	c.Thresholds.MemPct = 90
	c.Thresholds.SwapPct = 50
	c.CriticalOverridesQuiet = true
	c.Storage.Backend = "tsfile"
	c.Storage.RawRetention = "48h"
	c.Storage.RollupRetention = "720h"
	return c
}

func Load(path string) (*Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	// Unmarshal over defaults so present keys win, absent keys keep defaults.
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.SampleInterval == 0 {
		c.SampleInterval = 60
	}
	if c.FastInterval == 0 {
		c.FastInterval = 5
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 30
	}
	if c.BaselineSigma == 0 {
		c.BaselineSigma = 3
	}
	if c.Storage.Backend == "" {
		c.Storage.Backend = "tsfile"
	}
	if c.Storage.RawRetention == "" {
		c.Storage.RawRetention = "48h"
	}
	if c.Storage.RollupRetention == "" {
		c.Storage.RollupRetention = "720h"
	}
	return c, nil
}

func (c *Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: a crash mid-write must never truncate/corrupt the live file.
	// Write to a temp sibling, tighten perms (this file holds the plaintext
	// telegram token), then rename over the target. Rename is atomic on the
	// same filesystem, so readers see either the old or the new file, never a
	// partial one. os.WriteFile only applies the mode when it creates the file,
	// so Chmod the temp explicitly before the rename.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Get returns the effective string value for a dotted key.
func (c *Config) Get(key string) (string, bool) {
	switch key {
	case "sample_interval":
		return strconv.Itoa(c.SampleInterval), true
	case "fast_interval":
		return strconv.Itoa(c.FastInterval), true
	case "heartbeat_interval":
		return strconv.Itoa(c.HeartbeatInterval), true
	case "baseline_sigma":
		return trimFloat(c.BaselineSigma), true
	case "quiet_hours":
		return c.QuietHours, true
	case "telegram.token":
		return c.Telegram.Token, true
	case "telegram.chat_id":
		return c.Telegram.ChatID, true
	case "healthchecks.url":
		return c.Healthchecks.URL, true
	case "schedule.daily":
		return c.Schedule.Daily, true
	case "schedule.weekly":
		return c.Schedule.Weekly, true
	case "thresholds.disk_pct":
		return trimFloat(c.Thresholds.DiskPct), true
	case "thresholds.temp_c":
		return trimFloat(c.Thresholds.TempC), true
	case "thresholds.cpu_pct":
		return trimFloat(c.Thresholds.CPUPct), true
	case "thresholds.mem_pct":
		return trimFloat(c.Thresholds.MemPct), true
	case "thresholds.swap_pct":
		return trimFloat(c.Thresholds.SwapPct), true
	case "critical_overrides_quiet":
		return strconv.FormatBool(c.CriticalOverridesQuiet), true
	case "storage.backend":
		return c.Storage.Backend, true
	case "storage.raw_retention":
		return c.Storage.RawRetention, true
	case "storage.rollup_retention":
		return c.Storage.RollupRetention, true
	}
	return "", false
}

func (c *Config) Set(key, val string) error {
	f := func() (float64, error) { return strconv.ParseFloat(val, 64) }
	switch key {
	case "sample_interval":
		n, err := strconv.Atoi(val)
		if err != nil || n < 5 {
			return fmt.Errorf("sample_interval must be an integer >= 5")
		}
		fi := c.effectiveFastInterval()
		if n%fi != 0 {
			return fmt.Errorf("sample_interval %d must be an integer multiple of fast_interval %d", n, fi)
		}
		c.SampleInterval = n
	case "fast_interval":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("fast_interval must be an integer >= 1")
		}
		if si := c.effectiveSampleInterval(); si%n != 0 {
			return fmt.Errorf("fast_interval %d would make sample_interval %d no longer a multiple of it; adjust sample_interval first", n, si)
		}
		c.FastInterval = n
	case "heartbeat_interval":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("heartbeat_interval must be an integer >= 1")
		}
		c.HeartbeatInterval = n
	case "baseline_sigma":
		v, err := f()
		if err != nil {
			return err
		}
		c.BaselineSigma = v
	case "quiet_hours":
		if err := validateQuietHours(val); err != nil {
			return err
		}
		c.QuietHours = val
	case "telegram.token":
		c.Telegram.Token = val
	case "telegram.chat_id":
		c.Telegram.ChatID = val
	case "healthchecks.url":
		c.Healthchecks.URL = val
	case "schedule.daily":
		if err := validateDaily(val); err != nil {
			return err
		}
		c.Schedule.Daily = val
	case "schedule.weekly":
		if err := validateWeekly(val); err != nil {
			return err
		}
		c.Schedule.Weekly = val
	case "thresholds.disk_pct":
		v, err := f()
		if err != nil {
			return err
		}
		c.Thresholds.DiskPct = v
	case "thresholds.temp_c":
		v, err := f()
		if err != nil {
			return err
		}
		c.Thresholds.TempC = v
	case "thresholds.cpu_pct":
		v, err := f()
		if err != nil {
			return err
		}
		c.Thresholds.CPUPct = v
	case "thresholds.mem_pct":
		v, err := f()
		if err != nil {
			return err
		}
		c.Thresholds.MemPct = v
	case "thresholds.swap_pct":
		v, err := f()
		if err != nil {
			return err
		}
		c.Thresholds.SwapPct = v
	case "critical_overrides_quiet":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return err
		}
		c.CriticalOverridesQuiet = b
	case "storage.backend":
		if err := validateStorageBackend(val); err != nil {
			return err
		}
		c.Storage.Backend = val
	case "storage.raw_retention":
		if err := validateRetentionDuration("storage.raw_retention", val); err != nil {
			return err
		}
		c.Storage.RawRetention = val
	case "storage.rollup_retention":
		if err := validateRetentionDuration("storage.rollup_retention", val); err != nil {
			return err
		}
		c.Storage.RollupRetention = val
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

// effectiveFastInterval returns c.FastInterval, or the baked-in default (5)
// if the receiver is a zero-value Config (e.g. constructed directly rather
// than via Default()/Load()) so sample_interval validation never divides by
// zero.
func (c *Config) effectiveFastInterval() int {
	if c.FastInterval <= 0 {
		return 5
	}
	return c.FastInterval
}

// effectiveSampleInterval mirrors effectiveFastInterval for the slow tier: a
// zero-value SampleInterval (bare &Config{}) will be back-filled to 60 by
// Load(), so fast_interval validation must check against that same 60 rather
// than skipping the multiple check and letting Load() persist an inconsistent
// config.
func (c *Config) effectiveSampleInterval() int {
	if c.SampleInterval <= 0 {
		return 60
	}
	return c.SampleInterval
}

// Unset resets a key to its default by copying the default value into the field.
func (c *Config) Unset(key string) error {
	d := Default()
	v, ok := d.Get(key)
	if !ok {
		return fmt.Errorf("unknown key %q", key)
	}
	return c.Set(key, v)
}

func (c *Config) TargetEnabled(target string) bool {
	if c.Targets == nil {
		return true
	}
	return !c.Targets[target].Disabled
}

func (c *Config) SetTarget(target string, enabled bool) {
	if c.Targets == nil {
		c.Targets = map[string]TargetOverride{}
	}
	o := c.Targets[target]
	o.Disabled = !enabled
	c.Targets[target] = o
}

func (c *Config) TargetThreshold(target string) (float64, bool) {
	if c.Targets == nil {
		return 0, false
	}
	o, ok := c.Targets[target]
	if !ok || o.Threshold == nil {
		return 0, false
	}
	return *o.Threshold, true
}

func (c *Config) SetTargetThreshold(target string, v float64) {
	if c.Targets == nil {
		c.Targets = map[string]TargetOverride{}
	}
	o := c.Targets[target]
	o.Threshold = &v
	c.Targets[target] = o
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// parseHM validates an "HH:MM" clock string (00-23:00-59). Used by schedule keys.
func parseHM(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("want HH:MM")
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return fmt.Errorf("hour must be 00-23")
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return fmt.Errorf("minute must be 00-59")
	}
	return nil
}

var daysOfWeek = map[string]bool{
	"sun": true, "mon": true, "tue": true, "wed": true,
	"thu": true, "fri": true, "sat": true,
}

// validateDaily accepts "" (cleared) or "HH:MM".
func validateDaily(s string) error {
	if s == "" {
		return nil
	}
	if err := parseHM(s); err != nil {
		return fmt.Errorf("schedule.daily %q invalid: %v (want HH:MM or empty)", s, err)
	}
	return nil
}

// validateWeekly accepts "" (cleared) or "dow@HH:MM" (dow in sun..sat).
func validateWeekly(s string) error {
	if s == "" {
		return nil
	}
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 || !daysOfWeek[strings.ToLower(parts[0])] {
		return fmt.Errorf("schedule.weekly %q invalid: want dow@HH:MM (dow in sun..sat) or empty", s)
	}
	if err := parseHM(parts[1]); err != nil {
		return fmt.Errorf("schedule.weekly %q invalid: %v (want dow@HH:MM or empty)", s, err)
	}
	return nil
}

// validateQuietHours accepts "" (cleared) or "H-H"/"HH-HH" with each hour 0-23.
func validateQuietHours(s string) error {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return fmt.Errorf("quiet_hours %q invalid: want H-H (0-23) or empty", s)
	}
	for _, p := range parts {
		h, err := strconv.Atoi(p)
		if err != nil || h < 0 || h > 23 {
			return fmt.Errorf("quiet_hours %q invalid: hours must be 0-23 or empty", s)
		}
	}
	return nil
}
