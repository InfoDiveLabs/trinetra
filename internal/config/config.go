// Package config is the CLI-managed configuration store for serverwatch.
// There is no env or hand-edited file: all keys are set via `serverwatch config set`.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config is persisted as JSON. Zero values mean "use default"; Get resolves
// the effective value by falling back to Default() for unset scalar keys.
type Config struct {
	SampleInterval int     `json:"sample_interval,omitempty"` // seconds
	BaselineSigma  float64 `json:"baseline_sigma,omitempty"`
	QuietHours     string  `json:"quiet_hours,omitempty"` // "23-8" or ""
	Telegram       struct {
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
}

type TargetOverride struct {
	Disabled  bool     `json:"disabled,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// Default returns the baked-in defaults. A fresh install works with only a token.
func Default() *Config {
	c := &Config{
		SampleInterval: 60,
		BaselineSigma:  3,
	}
	c.Thresholds.DiskPct = 90
	c.Thresholds.TempC = 80
	c.Thresholds.CPUPct = 95
	c.Thresholds.MemPct = 90
	c.Thresholds.SwapPct = 50
	c.CriticalOverridesQuiet = true
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
	if c.BaselineSigma == 0 {
		c.BaselineSigma = 3
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
	return os.WriteFile(path, b, 0o600)
}

// Get returns the effective string value for a dotted key.
func (c *Config) Get(key string) (string, bool) {
	switch key {
	case "sample_interval":
		return strconv.Itoa(c.SampleInterval), true
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
		c.SampleInterval = n
	case "baseline_sigma":
		v, err := f()
		if err != nil {
			return err
		}
		c.BaselineSigma = v
	case "quiet_hours":
		c.QuietHours = val
	case "telegram.token":
		c.Telegram.Token = val
	case "telegram.chat_id":
		c.Telegram.ChatID = val
	case "healthchecks.url":
		c.Healthchecks.URL = val
	case "schedule.daily":
		c.Schedule.Daily = val
	case "schedule.weekly":
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
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
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
