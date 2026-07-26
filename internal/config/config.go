// Package config is the CLI-managed configuration store for serverwatch.
// There is no env or hand-edited file: all keys are set via `serverwatch config set`.
package config

import (
	"encoding/json"
	"fmt"
	"net"
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
	// BaselineMinPct is the minimum relative deviation (fraction of the
	// baseline mean, e.g. 0.15 = 15%) a value must ALSO clear -- alongside
	// BaselineSigma -- before a baseline (non-threshold) anomaly fires. It
	// exists because a noisy-but-stable metric's EWMA variance can be
	// underestimated, making a small, normal wobble read as many sigma from
	// the mean; requiring the value to also be materially far from the mean
	// in relative terms suppresses that flapping. Threshold-based alerts
	// (disk/docker/etc.) are unaffected.
	BaselineMinPct float64 `json:"baseline_min_pct,omitempty"`
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
	// Collect holds opt-in toggles for the expensive extended collectors
	// (docs/ROADMAP.md Epic #69). A nil pointer means "unset -> use the
	// documented default" so an explicit false survives Save/Load: a plain
	// bool with `omitempty` would drop a false value from the JSON and Load
	// would then re-fill it from Default() (true) instead of honoring it.
	Collect struct {
		// ContainerStats gates the slow-tier `docker stats` collector
		// (internal/serverwatch/docker.go dockerAccess.stats). Defaults to
		// true; nil is treated as true everywhere it's read.
		ContainerStats *bool `json:"container_stats,omitempty"`
		// NetThroughput gates the slow-tier per-interface network throughput
		// collector (internal/serverwatch/net.go NetRateCalc, /proc/net/dev
		// deltas -> bytes/sec). Defaults to true; nil is treated as true
		// everywhere it's read.
		NetThroughput *bool `json:"net_throughput,omitempty"`
		// Services gates the slow-tier full systemd unit inventory collector
		// (internal/serverwatch/discover.go listUnits/parseUnits, snapshot
		// -only — never persisted as a SampleStore series). Defaults to
		// true; nil is treated as true everywhere it's read. The existing
		// `systemctl --failed` alerting collection is separate and always
		// runs regardless of this setting.
		Services *bool `json:"services,omitempty"`
		// Processes gates the slow-tier process-table overview collector
		// (internal/serverwatch/proc.go collectProcesses: counts + top-N by
		// CPU/mem, for the Monitoring "processes" tab). Snapshot-only — never
		// persisted as a SampleStore series (per-process cardinality).
		// Defaults to true; nil is treated as true everywhere it's read.
		Processes *bool `json:"processes,omitempty"`
		// SmartAttrs gates the slow-tier per-device `smartctl -A` attribute
		// reads (internal/serverwatch/daemon.go collectSlow, feeding the
		// "smart:<dev>:temp" series) — the heaviest optional per-device call.
		// Defaults to true; nil is treated as true everywhere it's read. The
		// cheaper `smartctl --scan`/`-H` health checks are unaffected and
		// always run regardless of this setting.
		SmartAttrs *bool `json:"smart_attrs,omitempty"`
		// SmartInterval is the minimum seconds between SMART scans (smartctl
		// --scan/-H/-A). SMART health changes rarely and the scan is the
		// heaviest slow-tier call, so it is throttled independently of
		// sample_interval. Unset/0 -> default 1800s (30 min).
		SmartInterval int `json:"smart_interval,omitempty"`
	} `json:"collect"`
	// Web holds the embedded web UI server's settings (internal/web,
	// `-tags web` builds only — see docs/ROADMAP.md epic #56). The default
	// !web build never reads these, but the keys live here (untagged) so
	// they're manageable via `serverwatch config set` regardless of which
	// binary is installed.
	Web struct {
		// Enabled toggles the embedded web server. Defaults to false: the
		// web UI is opt-in even in the serverwatch-web binary.
		Enabled bool `json:"enabled,omitempty"`
		// Listen is the "host:port" the web server binds, validated with
		// net.SplitHostPort. Defaults to 127.0.0.1:8088 (localhost-only;
		// front it with a reverse proxy for LAN/WAN exposure).
		Listen string `json:"listen,omitempty"`
		// Mode selects how internal/web binds/serves: "proxy" (plain HTTP,
		// origin trusted from a local reverse proxy's X-Forwarded-* headers),
		// "autocert" (built-in Let's Encrypt via
		// golang.org/x/crypto/acme/autocert), or "manual" (TLSCert/TLSKey).
		// One of validWebModes; defaults to "proxy". See
		// internal/web/serving.go (issue #59).
		Mode string `json:"mode,omitempty"`
		// RPID is the WebAuthn relying party ID: the public hostname
		// passkeys are scoped to (no scheme/port). Required in autocert/
		// manual modes; may be left empty in proxy mode, where it/Origin are
		// instead derived per-request from the trusted reverse proxy's
		// forwarded headers.
		RPID string `json:"rp_id,omitempty"`
		// Origin is the full public origin ("https://host[:port]") passkey
		// ceremonies validate the browser's reported origin against. Its
		// host must match RPID exactly. Required in autocert/manual modes;
		// see RPID above for proxy mode.
		Origin string `json:"origin,omitempty"`
		// AutocertDomains is a comma-separated allowlist of hostnames
		// autocert.Manager's HostPolicy will request/renew certificates for.
		// Required (non-empty) in autocert mode.
		AutocertDomains string `json:"autocert_domains,omitempty"`
		// TLSCert/TLSKey are PEM file paths http.Server.ServeTLS loads in
		// manual mode. Both required (non-empty) in manual mode.
		TLSCert string `json:"tls_cert,omitempty"`
		TLSKey  string `json:"tls_key,omitempty"`
		// SessionTTL is a duration string (time.ParseDuration syntax, e.g.
		// "24h") controlling how long a signed-in web session stays valid.
		// Defaults to "24h".
		SessionTTL string `json:"session_ttl,omitempty"`
	} `json:"web"`
	// Public holds the admin-curated exposure settings for the anonymous
	// /public status page (internal/web, `-tags web` builds only -- see
	// docs/ROADMAP.md issue #67). Both fields default to "off"/empty:
	// nothing is exposed anonymously until an admin explicitly enables it
	// AND curates which panels are visible.
	Public struct {
		// Enabled toggles GET /public. Defaults to false; when false the
		// route 404s rather than revealing that a public page exists.
		Enabled bool `json:"enabled,omitempty"`
		// Panels is the server-side-enforced allowlist of panel/metric ids
		// exposed on /public -- e.g. "cpu", "mem", "disk:/", "uptime". Only
		// ids validatePublicPanel accepts may ever be stored here;
		// internal/web builds the public page by iterating exactly this
		// list against the live snapshot, so a metric absent from it can
		// never appear on the page even though it's present elsewhere.
		Panels []string `json:"panels,omitempty"`
	} `json:"public"`
}

// ContainerStatsEnabled reports whether the docker-stats collector
// (collect.container_stats) is enabled: unset (nil) defaults to true.
func (c *Config) ContainerStatsEnabled() bool {
	return c.Collect.ContainerStats == nil || *c.Collect.ContainerStats
}

// NetThroughputEnabled reports whether the per-interface network throughput
// collector (collect.net_throughput) is enabled: unset (nil) defaults to true.
func (c *Config) NetThroughputEnabled() bool {
	return c.Collect.NetThroughput == nil || *c.Collect.NetThroughput
}

// ServicesEnabled reports whether the systemd unit inventory collector
// (collect.services) is enabled: unset (nil) defaults to true.
func (c *Config) ServicesEnabled() bool {
	return c.Collect.Services == nil || *c.Collect.Services
}

// ProcessesEnabled reports whether the process-table overview collector
// (collect.processes) is enabled: unset (nil) defaults to true.
func (c *Config) ProcessesEnabled() bool {
	return c.Collect.Processes == nil || *c.Collect.Processes
}

// SmartAttrsEnabled reports whether the per-device SMART attribute reads
// collector (collect.smart_attrs) is enabled: unset (nil) defaults to true.
func (c *Config) SmartAttrsEnabled() bool {
	return c.Collect.SmartAttrs == nil || *c.Collect.SmartAttrs
}

// SmartIntervalSec is the effective SMART-scan throttle in seconds; unset/<=0
// defaults to 1800 (30 min). Set it as low as sample_interval to scan every slow tick.
func (c *Config) SmartIntervalSec() int {
	if c.Collect.SmartInterval <= 0 {
		return 1800
	}
	return c.Collect.SmartInterval
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

// validateListen rejects anything net.SplitHostPort can't parse into a
// host/port pair — the same "host:port" shape http.Server.Addr expects.
func validateListen(s string) error {
	if _, _, err := net.SplitHostPort(s); err != nil {
		return fmt.Errorf("web.listen %q invalid: %w (want host:port)", s, err)
	}
	return nil
}

// validWebModes allowlists web.mode: see internal/web/serving.go (issue #59)
// for what each mode does.
var validWebModes = map[string]bool{"proxy": true, "autocert": true, "manual": true}

// validateWebMode rejects anything outside validWebModes (including "").
func validateWebMode(s string) error {
	if validWebModes[s] {
		return nil
	}
	return fmt.Errorf("web.mode %q invalid: want one of proxy|autocert|manual", s)
}

// validateSessionTTL rejects anything time.ParseDuration can't parse, plus
// non-positive durations, mirroring validateRetentionDuration's rules for
// storage.raw_retention/rollup_retention.
func validateSessionTTL(s string) error {
	return validateRetentionDuration("web.session_ttl", s)
}

// validPublicPanels is the fixed catalog of static panel ids public.panels
// may name (internal/web's public-view allowlist, issue #67).
// "disk:<mount>" is validated separately in validatePublicPanel since the
// mount suffix is dynamic (one entry per filesystem the daemon reports).
var validPublicPanels = map[string]bool{
	"cpu": true, "mem": true, "swap": true, "load": true, "temp": true,
	"uptime": true, "services": true, "containers": true, "net": true,
}

// validatePublicPanel rejects any panel id public.panels wouldn't
// recognize: one of validPublicPanels, or "disk:<mount>" with a non-empty
// mount suffix. This is the single source of truth for what may ever be
// written to public.panels — internal/web's /settings/public handler
// reuses it (via Set) rather than re-implementing the allowlist, and
// internal/web's /public handler only ever renders ids that passed this
// check, so a stray/malicious value can never reach that unauthenticated
// page.
func validatePublicPanel(s string) error {
	if validPublicPanels[s] {
		return nil
	}
	if strings.HasPrefix(s, "disk:") && s != "disk:" {
		return nil
	}
	return fmt.Errorf("public panel %q invalid: want one of cpu|mem|swap|load|temp|uptime|services|containers|net or disk:<mount>", s)
}

// parsePublicPanels parses a comma-separated public.panels value into a
// slice, trimming whitespace and dropping empty entries (mirroring
// splitKinds), validating every entry against validatePublicPanel. Returns
// the first validation error, if any — the caller (Set) must not persist a
// partially-valid list.
func parsePublicPanels(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if err := validatePublicPanel(p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
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
		BaselineMinPct:    0.15,
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
	c.Web.Listen = "127.0.0.1:8088"
	c.Web.Mode = "proxy"
	c.Web.SessionTTL = "24h"
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
	if c.BaselineMinPct == 0 {
		c.BaselineMinPct = 0.15
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
	if c.Web.Listen == "" {
		c.Web.Listen = "127.0.0.1:8088"
	}
	if c.Web.Mode == "" {
		c.Web.Mode = "proxy"
	}
	if c.Web.SessionTTL == "" {
		c.Web.SessionTTL = "24h"
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
	case "baseline_min_pct":
		return trimFloat(c.BaselineMinPct), true
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
	case "collect.container_stats":
		return strconv.FormatBool(c.ContainerStatsEnabled()), true
	case "collect.net_throughput":
		return strconv.FormatBool(c.NetThroughputEnabled()), true
	case "collect.services":
		return strconv.FormatBool(c.ServicesEnabled()), true
	case "collect.processes":
		return strconv.FormatBool(c.ProcessesEnabled()), true
	case "collect.smart_attrs":
		return strconv.FormatBool(c.SmartAttrsEnabled()), true
	case "collect.smart_interval":
		return strconv.Itoa(c.SmartIntervalSec()), true
	case "web.enabled":
		return strconv.FormatBool(c.Web.Enabled), true
	case "web.listen":
		return c.Web.Listen, true
	case "web.mode":
		return c.Web.Mode, true
	case "web.rp_id":
		return c.Web.RPID, true
	case "web.origin":
		return c.Web.Origin, true
	case "web.autocert_domains":
		return c.Web.AutocertDomains, true
	case "web.tls_cert":
		return c.Web.TLSCert, true
	case "web.tls_key":
		return c.Web.TLSKey, true
	case "web.session_ttl":
		return c.Web.SessionTTL, true
	case "public.enabled":
		return strconv.FormatBool(c.Public.Enabled), true
	case "public.panels":
		return strings.Join(c.Public.Panels, ","), true
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
	case "baseline_min_pct":
		v, err := f()
		if err != nil {
			return err
		}
		if v < 0 {
			return fmt.Errorf("baseline_min_pct must be >= 0")
		}
		c.BaselineMinPct = v
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
	case "collect.container_stats":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.container_stats: %w", err)
		}
		c.Collect.ContainerStats = &b
	case "collect.net_throughput":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.net_throughput: %w", err)
		}
		c.Collect.NetThroughput = &b
	case "collect.services":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.services: %w", err)
		}
		c.Collect.Services = &b
	case "collect.processes":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.processes: %w", err)
		}
		c.Collect.Processes = &b
	case "collect.smart_attrs":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.smart_attrs: %w", err)
		}
		c.Collect.SmartAttrs = &b
	case "collect.smart_interval":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("collect.smart_interval must be an integer >= 1")
		}
		c.Collect.SmartInterval = n
	case "web.enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("web.enabled: %w", err)
		}
		c.Web.Enabled = b
	case "web.listen":
		if err := validateListen(val); err != nil {
			return err
		}
		c.Web.Listen = val
	case "web.mode":
		if err := validateWebMode(val); err != nil {
			return err
		}
		c.Web.Mode = val
	case "web.rp_id":
		c.Web.RPID = val
	case "web.origin":
		c.Web.Origin = val
	case "web.autocert_domains":
		c.Web.AutocertDomains = val
	case "web.tls_cert":
		c.Web.TLSCert = val
	case "web.tls_key":
		c.Web.TLSKey = val
	case "web.session_ttl":
		if err := validateSessionTTL(val); err != nil {
			return err
		}
		c.Web.SessionTTL = val
	case "public.enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("public.enabled: %w", err)
		}
		c.Public.Enabled = b
	case "public.panels":
		panels, err := parsePublicPanels(val)
		if err != nil {
			return err
		}
		c.Public.Panels = panels
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
