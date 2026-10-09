// Package config is the CLI-managed configuration store for trinetra.
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

// Config is persisted as JSON.
type Config struct {
	// Name is this host's display name/id (server.name).
	Name string `json:"name,omitempty"`
	// SampleInterval is the slow tier: seconds between full baseline samples.
	SampleInterval int `json:"sample_interval,omitempty"`
	// FastInterval is the fast tier: seconds between lightweight checks.
	FastInterval int `json:"fast_interval,omitempty"`
	// HeartbeatInterval is the seconds between liveness heartbeats.
	HeartbeatInterval int `json:"heartbeat_interval,omitempty"`
	// ExecTimeout is the per-command hang-breaker for external collectors
	// (df/docker/systemctl/smartctl), in seconds.
	ExecTimeout   int     `json:"exec_timeout,omitempty"`
	BaselineSigma float64 `json:"baseline_sigma,omitempty"`
	// BaselineMinPct is the minimum relative deviation (fraction of the baseline mean, e.g.
	// 0.15 = 15%) a value must also clear, alongside BaselineSigma.
	BaselineMinPct float64 `json:"baseline_min_pct,omitempty"`
	// BaselineAlerts gates the baseline (z-score) deviation branch of anomaly evaluation;
	// threshold-based alerting is unaffected and always on.
	BaselineAlerts bool   `json:"baseline_alerts,omitempty"`
	QuietHours     string `json:"quiet_hours,omitempty"` // "23-8" or ""
	Setup          struct {
		Completed bool `json:"completed,omitempty"`
	} `json:"setup"`
	Telegram struct {
		Token  string `json:"token,omitempty"`
		ChatID string `json:"chat_id,omitempty"`
		// MaxEnrollAttempts is how many consecutive wrong "/start <pin>" guesses an unclaimed bot
		// tolerates before the PIN cools down and rotates (#93).
		MaxEnrollAttempts int `json:"enroll_max_attempts,omitempty"`
		// EnrollCooldown is the seconds "/start" attempts are ignored after the threshold
		// is hit, during which the PIN also rotates (#93). Unset/<=0 means 60.
		EnrollCooldown int `json:"enroll_cooldown,omitempty"`
	} `json:"telegram"`
	Healthchecks struct {
		URL string `json:"url,omitempty"`
	} `json:"healthchecks"`
	Notify struct {
		// BlockPrivateTargets, when true, refuses to dial loopback/link-local (incl.
		// 169.254.169.254 metadata)/private targets for URLs the daemon calls for the operator.
		BlockPrivateTargets bool `json:"block_private_targets,omitempty"`
	} `json:"notify"`
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
	// `trinetra channel add|list|remove|set|test`.
	Channels []ChannelConfig `json:"channels,omitempty"`
	Storage  struct {
		// Backend selects the SampleStore implementation: one of validStorageBackends,
		// default "tsfile".
		Backend string `json:"backend,omitempty"`
		// RawRetention/RollupRetention are time.ParseDuration strings for how long the tsfile
		// backend keeps raw and 1m-rollup samples; event retention reuses RollupRetention.
		RawRetention    string `json:"raw_retention,omitempty"`
		RollupRetention string `json:"rollup_retention,omitempty"`
	} `json:"storage"`
	// Collect holds opt-in toggles for the expensive extended collectors.
	Collect struct {
		// ContainerStats gates the slow-tier `docker stats` collector.
		ContainerStats *bool `json:"container_stats,omitempty"`
		// NetThroughput gates the slow-tier per-interface throughput collector
		// (/proc/net/dev deltas). Default true; nil reads as true.
		NetThroughput *bool `json:"net_throughput,omitempty"`
		// Services gates the slow-tier full systemd unit inventory (snapshot-only, never a
		// SampleStore series).
		Services *bool `json:"services,omitempty"`
		// Processes gates the slow-tier process-table overview (counts + top-N, for the
		// Monitoring "processes" tab).
		Processes *bool `json:"processes,omitempty"`
		// SmartAttrs gates the slow-tier per-device `smartctl -A` reads (the "smart:<dev>: temp"
		// series), the heaviest optional call.
		SmartAttrs *bool `json:"smart_attrs,omitempty"`
		// SmartInterval is the minimum seconds between SMART scans.
		SmartInterval int `json:"smart_interval,omitempty"`
		// PublicIP gates the host's public-IP lookup (#102), an outbound call to a third-party
		// echo service.
		PublicIP *bool `json:"public_ip,omitempty"`
	} `json:"collect"`
	// Web holds the web UI server's settings (trinetra-web).
	Web struct {
		// Enabled toggles the web server.
		Enabled bool `json:"enabled,omitempty"`
		// Listen is the "host:port" the web server binds, validated with net.SplitHostPort.
		Listen string `json:"listen,omitempty"`
		// Mode selects how internal/web serves: "proxy" (plain HTTP, origin trusted from a local
		// reverse proxy's X-Forwarded-* headers), "autocert" (Let's Encrypt via autocert).
		Mode string `json:"mode,omitempty"`
		// RPID is the WebAuthn relying party ID: the public hostname passkeys are scoped to (no
		// scheme/port).
		RPID string `json:"rp_id,omitempty"`
		// Origin is the full public origin ("https://host[:port]") that passkey ceremonies
		// validate the browser's origin against; its host must equal RPID.
		Origin string `json:"origin,omitempty"`
		// AutocertDomains is a comma-separated allowlist of hostnames autocert's
		// HostPolicy will request certificates for. Required in autocert mode.
		AutocertDomains string `json:"autocert_domains,omitempty"`
		// TLSCert/TLSKey are PEM file paths for ServeTLS. Both required in manual mode.
		TLSCert string `json:"tls_cert,omitempty"`
		TLSKey  string `json:"tls_key,omitempty"`
		// SessionTTL is a time.ParseDuration string for how long a web session stays valid.
		SessionTTL string `json:"session_ttl,omitempty"`
	} `json:"web"`
	// Public holds the admin-curated settings for the anonymous /public status page.
	Public struct {
		// Enabled toggles GET /public.
		Enabled bool `json:"enabled,omitempty"`
		// Panels is the server-side allowlist of panel ids shown on /public, e.g. "cpu",
		// "disk:/", "uptime".
		Panels []string `json:"panels,omitempty"`
	} `json:"public"`
	// Status is the public status page (issue #157): the page title, how long a recovered
	// automatic incident waits before resolving itself ("0" = never).
	Status struct {
		Title            string   `json:"title,omitempty"`
		AutoResolveAfter string   `json:"auto_resolve_after,omitempty"`
		EchoChannels     []string `json:"echo_channels,omitempty"`
	} `json:"status"`
	// Fleet holds master/child fleet settings (fleet mode, see
	// docs/handbook/02-architecture.md "Fleet mode").
	Fleet struct {
		Role              string `json:"role,omitempty"`
		Listen            string `json:"listen,omitempty"`
		Address           string `json:"address,omitempty"`
		MasterURL         string `json:"master_url,omitempty"`
		CAPin             string `json:"ca_pin,omitempty"`
		NodeID            string `json:"node_id,omitempty"`
		OutboxMaxMB       int    `json:"outbox_max_mb,omitempty"`
		NodeDownAfter     string `json:"node_down_after,omitempty"`
		FallbackAfter     string `json:"fallback_after,omitempty"`
		LinkDownWarnAfter string `json:"link_down_warn_after,omitempty"`
	} `json:"fleet"`
	// Update configures signed self-update (docs/handbook/10-operations.md).
	Update struct {
		Channel       string `json:"channel,omitempty"`        // stable | beta | off; "" = stable
		Source        string `json:"source,omitempty"`         // github | none; "" = github
		GitHubToken   string `json:"github_token,omitempty"`   // read-only token for a private repo
		CheckInterval string `json:"check_interval,omitempty"` // Go duration >= 1h; "" = 24h
	} `json:"update"`
}

// KeepFleetIdentity copies the fleet identity keys (role, address, master URL, CA pin, node
// id) from onDisk into c.
func (c *Config) KeepFleetIdentity(onDisk *Config) {
	c.Fleet.Role = onDisk.Fleet.Role
	c.Fleet.Address = onDisk.Fleet.Address
	c.Fleet.MasterURL = onDisk.Fleet.MasterURL
	c.Fleet.CAPin = onDisk.Fleet.CAPin
	c.Fleet.NodeID = onDisk.Fleet.NodeID
}

// ContainerStatsEnabled reports whether the docker-stats collector
// (collect.container_stats) is enabled: unset (nil) defaults to true.
func (c *Config) ContainerStatsEnabled() bool {
	return c.Collect.ContainerStats == nil || *c.Collect.ContainerStats
}

// PublicIPEnabled reports whether the opt-in public-IP lookup (collect.public_ip)
// is enabled. It defaults to FALSE (nil is false) because it makes an outbound call.
func (c *Config) PublicIPEnabled() bool {
	return c.Collect.PublicIP != nil && *c.Collect.PublicIP
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

// SmartIntervalSec is the effective SMART-scan throttle in seconds; unset/<=0 means 1800.
func (c *Config) SmartIntervalSec() int {
	if c.Collect.SmartInterval <= 0 {
		return 1800
	}
	return c.Collect.SmartInterval
}

// EnrollMaxAttempts is the effective number of consecutive wrong "/start <pin>"
// guesses tolerated before the PIN cools down and rotates; default 5 (#93).
func (c *Config) EnrollMaxAttempts() int {
	if c.Telegram.MaxEnrollAttempts <= 0 {
		return 5
	}
	return c.Telegram.MaxEnrollAttempts
}

// EnrollCooldownSec is the effective seconds "/start" attempts are ignored after
// the threshold is hit (the PIN rotates then too); default 60 (#93).
func (c *Config) EnrollCooldownSec() int {
	if c.Telegram.EnrollCooldown <= 0 {
		return 60
	}
	return c.Telegram.EnrollCooldown
}

// Fleet roles. An empty Fleet.Role is treated as RoleSolo.
const (
	RoleSolo   = "solo"
	RoleMaster = "master"
	RoleChild  = "child"
)

// FleetRole is the effective fleet role; empty means solo.
func (c *Config) FleetRole() string {
	switch c.Fleet.Role {
	case RoleMaster, RoleChild:
		return c.Fleet.Role
	}
	return RoleSolo
}

// FleetListen is the master's fleet listener address; default ":9443".
func (c *Config) FleetListen() string {
	if c.Fleet.Listen == "" {
		return ":9443"
	}
	return c.Fleet.Listen
}

// FleetOutboxMaxBytes is the child's outbox cap; default 512 MiB.
func (c *Config) FleetOutboxMaxBytes() int64 {
	if c.Fleet.OutboxMaxMB <= 0 {
		return 512 << 20
	}
	return int64(c.Fleet.OutboxMaxMB) << 20
}

// FleetNodeDownAfter is how long the master waits without contact before
// declaring a node down; default 2m. An unparsable stored value also falls back.
func (c *Config) FleetNodeDownAfter() time.Duration {
	if d, err := time.ParseDuration(c.Fleet.NodeDownAfter); err == nil && d > 0 {
		return d
	}
	return 2 * time.Minute
}

// StatusTitle is the public status page heading (default "Status").
func (c *Config) StatusTitle() string {
	if t := strings.TrimSpace(c.Status.Title); t != "" {
		return t
	}
	return "Status"
}

// StatusAutoResolveAfter is how long a recovered automatic incident waits
// before resolving itself; 0 means never (default 24h).
func (c *Config) StatusAutoResolveAfter() time.Duration {
	if c.Status.AutoResolveAfter == "0" {
		return 0
	}
	if d, err := time.ParseDuration(c.Status.AutoResolveAfter); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

// FleetFallbackAfter is how long a child waits for the master's receipt of a routed alert
// before delivering locally ("via local fallback: master unreachable"); default 2m.
func (c *Config) FleetFallbackAfter() time.Duration {
	if d, err := time.ParseDuration(c.Fleet.FallbackAfter); err == nil && d > 0 {
		return d
	}
	return 2 * time.Minute
}

// FleetLinkDownWarnAfter is how long a child's link to the master must be down before it
// raises its own "fleet link down" warning; default 10m.
func (c *Config) FleetLinkDownWarnAfter() time.Duration {
	if d, err := time.ParseDuration(c.Fleet.LinkDownWarnAfter); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// UpdateChannel is the effective release channel for signed self-update:
// unset ("") defaults to "stable". Set validates against stable|beta|off.
func (c *Config) UpdateChannel() string {
	if c.Update.Channel == "" {
		return "stable"
	}
	return c.Update.Channel
}

// UpdateSource is the effective self-update source: unset ("") defaults to "github".
func (c *Config) UpdateSource() string {
	if c.Update.Source == "" {
		return "github"
	}
	return c.Update.Source
}

// UpdateCheckInterval is the effective interval between self-update checks: unset,
// unparsable or below the 1h floor means 24h.
func (c *Config) UpdateCheckInterval() time.Duration {
	if d, err := time.ParseDuration(c.Update.CheckInterval); err == nil && d >= time.Hour {
		return d
	}
	return 24 * time.Hour
}

// fleetManagedKeys are readable via Get but written only by the `trinetra fleet` commands.
var fleetManagedKeys = map[string]bool{
	"fleet.role": true, "fleet.address": true, "fleet.master_url": true,
	"fleet.ca_pin": true, "fleet.node_id": true,
}

type TargetOverride struct {
	Disabled  bool     `json:"disabled,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// ChannelConfig describes one notification channel.
type ChannelConfig struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Enabled bool   `json:"enabled"`
	// Settings holds type-specific key/value config, e.g. {"chat_id": "..."}.
	Settings map[string]string `json:"settings,omitempty"`
	// MinSeverity is "info" | "warning" | "critical".
	MinSeverity            string   `json:"min_severity,omitempty"`
	IncludeKinds           []string `json:"include_kinds,omitempty"`
	ExcludeKinds           []string `json:"exclude_kinds,omitempty"`
	CriticalOverridesQuiet bool     `json:"critical_overrides_quiet,omitempty"`
}

// validSeverities mirrors trinetra.Severity's string form; config cannot import
// trinetra (import cycle), so it validates severities itself.
var validSeverities = map[string]bool{"info": true, "warning": true, "critical": true}

// validStorageBackends allowlists storage.backend: "tsfile" is the default and
// "memory" the in-memory reference SampleStore.
var validStorageBackends = map[string]bool{"tsfile": true, "memory": true}

// validateStorageBackend rejects anything outside validStorageBackends.
func validateStorageBackend(s string) error {
	if validStorageBackends[s] {
		return nil
	}
	return fmt.Errorf("storage.backend %q invalid: want one of tsfile|memory", s)
}

// validateRetentionDuration rejects unparsable durations and non-positive ones
// (a zero or negative retention would mean "keep nothing").
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

// validateListen rejects anything net.SplitHostPort cannot parse into host/port.
func validateListen(s string) error {
	if _, _, err := net.SplitHostPort(s); err != nil {
		return fmt.Errorf("web.listen %q invalid: %w (want host:port)", s, err)
	}
	return nil
}

// validWebModes allowlists web.mode.
var validWebModes = map[string]bool{"proxy": true, "autocert": true, "manual": true}

// validateWebMode rejects anything outside validWebModes (including "").
func validateWebMode(s string) error {
	if validWebModes[s] {
		return nil
	}
	return fmt.Errorf("web.mode %q invalid: want one of proxy|autocert|manual", s)
}

// validateSessionTTL rejects unparsable and non-positive durations, like
// validateRetentionDuration.
func validateSessionTTL(s string) error {
	return validateRetentionDuration("web.session_ttl", s)
}

// validPublicPanels is the fixed catalog of static ids public.panels may name.
var validPublicPanels = map[string]bool{
	"availability": true,
	"cpu":          true, "mem": true, "swap": true, "load": true, "temp": true,
	"uptime": true, "services": true, "containers": true, "net": true,
}

// validatePublicPanel rejects any panel id public.panels would not recognize: one of
// validPublicPanels, or "disk:<mount>" with a non-empty mount.
func validatePublicPanel(s string) error {
	if validPublicPanels[s] {
		return nil
	}
	if strings.HasPrefix(s, "disk:") && s != "disk:" {
		return nil
	}
	return fmt.Errorf("public panel %q invalid: want one of availability|cpu|mem|swap|load|temp|uptime|services|containers|net or disk:<mount>", s)
}

// parsePublicPanels parses a comma-separated public.panels value, trimming whitespace and
// dropping empty entries.
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

// AddChannel appends a new channel.
func (c *Config) AddChannel(cc ChannelConfig) {
	c.Channels = append(c.Channels, cc)
}

// RemoveChannel deletes the channel named name, reporting whether one was found.
func (c *Config) RemoveChannel(name string) bool {
	for i, cc := range c.Channels {
		if cc.Name == name {
			c.Channels = append(c.Channels[:i], c.Channels[i+1:]...)
			return true
		}
	}
	return false
}

// GetChannel returns a pointer to the named channel's config (so callers such as
// SetChannelField can mutate it in place).
func (c *Config) GetChannel(name string) (*ChannelConfig, bool) {
	for i := range c.Channels {
		if c.Channels[i].Name == name {
			return &c.Channels[i], true
		}
	}
	return nil, false
}

// splitKinds parses a comma-separated kind list, trimming whitespace and dropping empty
// elements.
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

// SetChannelField updates one field of an existing channel by name.
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

// ServerName returns the effective display name: server.name when set, else the system
// hostname, else "trinetra".
func (c *Config) ServerName() string {
	if c.Name != "" {
		return c.Name
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "trinetra"
}

// Default returns the baked-in defaults. A fresh install works with only a token.
func Default() *Config {
	c := &Config{
		SampleInterval:    60,
		FastInterval:      5,
		HeartbeatInterval: 30,
		ExecTimeout:       60,
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
	if c.ExecTimeout == 0 {
		c.ExecTimeout = 60
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
	// Atomic write: a crash mid-write must not corrupt the live file.
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
	case "server.name":
		return c.ServerName(), true
	case "sample_interval":
		return strconv.Itoa(c.SampleInterval), true
	case "fast_interval":
		return strconv.Itoa(c.FastInterval), true
	case "heartbeat_interval":
		return strconv.Itoa(c.HeartbeatInterval), true
	case "exec_timeout":
		return strconv.Itoa(c.ExecTimeout), true
	case "baseline_sigma":
		return trimFloat(c.BaselineSigma), true
	case "baseline_min_pct":
		return trimFloat(c.BaselineMinPct), true
	case "baseline_alerts":
		return strconv.FormatBool(c.BaselineAlerts), true
	case "quiet_hours":
		return c.QuietHours, true
	case "telegram.token":
		return c.Telegram.Token, true
	case "telegram.chat_id":
		return c.Telegram.ChatID, true
	case "telegram.enroll_max_attempts":
		return strconv.Itoa(c.EnrollMaxAttempts()), true
	case "telegram.enroll_cooldown":
		return strconv.Itoa(c.EnrollCooldownSec()), true
	case "healthchecks.url":
		return c.Healthchecks.URL, true
	case "notify.block_private_targets":
		return strconv.FormatBool(c.Notify.BlockPrivateTargets), true
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
	case "collect.public_ip":
		return strconv.FormatBool(c.PublicIPEnabled()), true
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
	case "setup.completed":
		return strconv.FormatBool(c.Setup.Completed), true
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
	case "status.title":
		return c.StatusTitle(), true
	case "status.auto_resolve_after":
		if d := c.StatusAutoResolveAfter(); d == 0 {
			return "0", true
		} else {
			return d.String(), true
		}
	case "status.echo_channels":
		return strings.Join(c.Status.EchoChannels, ","), true
	case "fleet.role":
		return c.FleetRole(), true
	case "fleet.listen":
		return c.FleetListen(), true
	case "fleet.address":
		return c.Fleet.Address, true
	case "fleet.master_url":
		return c.Fleet.MasterURL, true
	case "fleet.ca_pin":
		return c.Fleet.CAPin, true
	case "fleet.node_id":
		return c.Fleet.NodeID, true
	case "fleet.outbox_max_mb":
		return strconv.FormatInt(c.FleetOutboxMaxBytes()>>20, 10), true
	case "fleet.node_down_after":
		return c.FleetNodeDownAfter().String(), true
	case "fleet.fallback_after":
		return c.FleetFallbackAfter().String(), true
	case "fleet.link_down_warn_after":
		return c.FleetLinkDownWarnAfter().String(), true
	case "update.channel":
		return c.UpdateChannel(), true
	case "update.source":
		return c.UpdateSource(), true
	case "update.github_token":
		// Raw value: callers (cmdConfig) redact via IsSecretKey before printing.
		return c.Update.GitHubToken, true
	case "update.check_interval":
		if c.Update.CheckInterval == "" {
			return "24h", true
		}
		return c.Update.CheckInterval, true
	}
	return "", false
}

func (c *Config) Set(key, val string) error {
	if fleetManagedKeys[key] {
		return fmt.Errorf("%s is managed by `trinetra fleet init|join|leave|disable`, not config set", key)
	}
	f := func() (float64, error) { return strconv.ParseFloat(val, 64) }
	switch key {
	case "server.name":
		c.Name = val // empty clears back to the system hostname (see ServerName)
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
	case "exec_timeout":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("exec_timeout must be an integer >= 1")
		}
		c.ExecTimeout = n
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
	case "baseline_alerts":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("baseline_alerts: %w", err)
		}
		c.BaselineAlerts = b
	case "quiet_hours":
		if err := validateQuietHours(val); err != nil {
			return err
		}
		c.QuietHours = val
	case "telegram.token":
		c.Telegram.Token = val
	case "telegram.chat_id":
		c.Telegram.ChatID = val
	case "telegram.enroll_max_attempts":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("telegram.enroll_max_attempts must be an integer >= 1")
		}
		c.Telegram.MaxEnrollAttempts = n
	case "telegram.enroll_cooldown":
		n, err := strconv.Atoi(val)
		if err != nil || n < 1 {
			return fmt.Errorf("telegram.enroll_cooldown must be an integer >= 1 (seconds)")
		}
		c.Telegram.EnrollCooldown = n
	case "healthchecks.url":
		c.Healthchecks.URL = val
	case "notify.block_private_targets":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("notify.block_private_targets: %w", err)
		}
		c.Notify.BlockPrivateTargets = b
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
	case "collect.public_ip":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("collect.public_ip: %w", err)
		}
		c.Collect.PublicIP = &b
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
	case "setup.completed":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("setup.completed: %w", err)
		}
		c.Setup.Completed = b
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
	case "status.title":
		v := strings.TrimSpace(val)
		if len([]rune(v)) > 60 {
			return fmt.Errorf("status.title must be at most 60 characters")
		}
		c.Status.Title = v
	case "status.auto_resolve_after":
		if val != "0" {
			d, err := time.ParseDuration(val)
			if err != nil || d < time.Hour {
				return fmt.Errorf("status.auto_resolve_after must be 0 (never) or a duration of at least 1h, got %q", val)
			}
		}
		c.Status.AutoResolveAfter = val
	case "status.echo_channels":
		c.Status.EchoChannels = splitKinds(val)
	case "fleet.listen":
		if err := validateListen(val); err != nil {
			return fmt.Errorf("fleet.listen: %w", err)
		}
		c.Fleet.Listen = val
	case "fleet.outbox_max_mb":
		n, err := strconv.Atoi(val)
		if err != nil || n < 16 {
			return fmt.Errorf("fleet.outbox_max_mb must be an integer >= 16")
		}
		c.Fleet.OutboxMaxMB = n
	case "fleet.node_down_after":
		d, err := time.ParseDuration(val)
		if err != nil || d < 30*time.Second {
			return fmt.Errorf("fleet.node_down_after must be a duration >= 30s, e.g. 2m")
		}
		c.Fleet.NodeDownAfter = val
	case "fleet.fallback_after":
		d, err := time.ParseDuration(val)
		if err != nil || d < 5*time.Second {
			return fmt.Errorf("fleet.fallback_after must be a duration >= 5s, e.g. 2m")
		}
		c.Fleet.FallbackAfter = val
	case "fleet.link_down_warn_after":
		d, err := time.ParseDuration(val)
		if err != nil || d < 30*time.Second {
			return fmt.Errorf("fleet.link_down_warn_after must be a duration >= 30s, e.g. 10m")
		}
		c.Fleet.LinkDownWarnAfter = val
	case "update.channel":
		if val != "stable" && val != "beta" && val != "off" {
			return fmt.Errorf("update.channel %q invalid: want one of stable|beta|off", val)
		}
		c.Update.Channel = val
	case "update.source":
		if val != "github" && val != "none" {
			return fmt.Errorf("update.source %q invalid: want one of github|none", val)
		}
		c.Update.Source = val
	case "update.github_token":
		c.Update.GitHubToken = val
	case "update.check_interval":
		d, err := time.ParseDuration(val)
		if err != nil || d < time.Hour {
			return fmt.Errorf("update.check_interval %q invalid: want a duration >= 1h, e.g. 24h", val)
		}
		c.Update.CheckInterval = val
	default:
		return fmt.Errorf("unknown key %q", key)
	}
	return nil
}

// KeyInfo describes one flat, settable config key for trinetra-ctl's "all settings" screen
// (#91): a display Group, a value-Kind hint, a one-line Help.
type KeyInfo struct {
	Name            string
	Group           string
	Kind            string
	Help            string
	RestartRequired bool
	// Secret marks a credential that must never be printed in the clear: `config get`
	// and other displays show "(set)"/"(not set)". See IsSecretKey.
	Secret bool
}

// IsSecretKey reports whether a key holds a credential that must never be
// printed (CLI get, dumps, the web config page show "(set)" instead).
func IsSecretKey(name string) bool {
	for _, k := range Keys() {
		if k.Name == name {
			return k.Secret
		}
	}
	return false
}

// Keys returns the full catalog of flat, settable config keys, grouped for display.
func Keys() []KeyInfo {
	out := make([]KeyInfo, len(keyCatalog))
	copy(out, keyCatalog)
	return out
}

// keyCatalog is Keys' backing data, in the order the ctl settings screen shows it.
var keyCatalog = []KeyInfo{
	{Name: "sample_interval", Group: "Intervals", Kind: "int", Help: "Seconds between full baseline samples (the slow tier)."},
	{Name: "fast_interval", Group: "Intervals", Kind: "int", Help: "Seconds between lightweight checks (the fast tier)."},
	{Name: "heartbeat_interval", Group: "Intervals", Kind: "int", Help: "Seconds between liveness heartbeats."},
	{Name: "exec_timeout", Group: "Intervals", Kind: "int", Help: "Per-command hang-breaker for external collectors (df/docker/systemctl/smartctl), in seconds. Generous by design -- only a wedged command should hit it. Takes effect on restart."},

	{Name: "baseline_sigma", Group: "Baseline", Kind: "float", Help: "Standard deviations from the mean before a baseline anomaly fires."},
	{Name: "baseline_min_pct", Group: "Baseline", Kind: "float", Help: "Minimum relative deviation from the baseline mean also required to fire."},
	{Name: "baseline_alerts", Group: "Baseline", Kind: "bool", Help: "Enable baseline (z-score) deviation alerts, on top of threshold alerts."},

	{Name: "thresholds.cpu_pct", Group: "Thresholds", Kind: "float", Help: "Global CPU percent threshold for alerting."},
	{Name: "thresholds.mem_pct", Group: "Thresholds", Kind: "float", Help: "Global memory percent threshold for alerting."},
	{Name: "thresholds.swap_pct", Group: "Thresholds", Kind: "float", Help: "Global swap percent threshold for alerting."},
	{Name: "thresholds.temp_c", Group: "Thresholds", Kind: "float", Help: "Global temperature threshold in Celsius for alerting."},
	{Name: "thresholds.disk_pct", Group: "Thresholds", Kind: "float", Help: "Global disk usage percent threshold for alerting."},

	{Name: "critical_overrides_quiet", Group: "Alerting", Kind: "bool", Help: "Let disk-full-imminent style critical alerts bypass quiet hours."},
	{Name: "quiet_hours", Group: "Alerting", Kind: "string", Help: "Quiet hours window as H-H, e.g. 22-6, or empty to disable."},

	{Name: "telegram.token", Group: "Notifications", Kind: "string", Secret: true, Help: "Telegram bot token from @BotFather."},
	{Name: "telegram.chat_id", Group: "Notifications", Kind: "string", Help: "Telegram chat id enrolled to receive alerts."},
	{Name: "telegram.enroll_max_attempts", Group: "Notifications", Kind: "int", Help: "Wrong /start <pin> guesses tolerated before the enrollment PIN cools down and rotates (brute-force bound). Default 5."},
	{Name: "telegram.enroll_cooldown", Group: "Notifications", Kind: "int", Help: "Seconds /start attempts are ignored after the attempt limit is hit (the PIN also rotates then). Default 60."},
	{Name: "notify.block_private_targets", Group: "Notifications", Kind: "bool", Help: "Refuse to dial loopback/link-local(incl. 169.254.169.254)/private targets for channel + healthchecks URLs (SSRF hardening). Default false."},
	{Name: "healthchecks.url", Group: "Notifications", Kind: "string", Help: "healthchecks.io ping URL, or empty to disable."},

	{Name: "schedule.daily", Group: "Schedule", Kind: "string", Help: "Daily digest time as HH:MM, or empty to disable."},
	{Name: "schedule.weekly", Group: "Schedule", Kind: "string", Help: "Weekly digest time as dow@HH:MM, e.g. mon@09:00, or empty to disable."},

	{Name: "storage.backend", Group: "Storage", Kind: "enum", Help: "Sample store backend: tsfile or memory.", RestartRequired: true},
	{Name: "storage.raw_retention", Group: "Storage", Kind: "duration", Help: "How long raw samples are kept, e.g. 48h.", RestartRequired: true},
	{Name: "storage.rollup_retention", Group: "Storage", Kind: "duration", Help: "How long 1m-rollup samples and events are kept, e.g. 720h.", RestartRequired: true},

	{Name: "collect.container_stats", Group: "Collection", Kind: "bool", Help: "Collect per-container docker stats."},
	{Name: "collect.public_ip", Group: "Collection", Kind: "bool", Help: "Look up the host's public IP via an outbound call (opt-in, default false)."},
	{Name: "collect.net_throughput", Group: "Collection", Kind: "bool", Help: "Collect per-interface network throughput."},
	{Name: "collect.services", Group: "Collection", Kind: "bool", Help: "Collect the full systemd unit inventory."},
	{Name: "collect.processes", Group: "Collection", Kind: "bool", Help: "Collect the process-table overview."},
	{Name: "collect.smart_attrs", Group: "Collection", Kind: "bool", Help: "Collect per-device SMART attribute reads."},
	{Name: "collect.smart_interval", Group: "Collection", Kind: "int", Help: "Minimum seconds between SMART scans."},

	{Name: "setup.completed", Group: "Setup", Kind: "bool", Help: "Set once the guided first run in trinetra cli is finished or skipped."},

	{Name: "web.enabled", Group: "Web", Kind: "bool", Help: "Enable the web UI server.", RestartRequired: true},
	{Name: "web.listen", Group: "Web", Kind: "string", Help: "Web server bind address as host:port.", RestartRequired: true},
	{Name: "web.mode", Group: "Web", Kind: "enum", Help: "Web serving mode: proxy, autocert, or manual."},
	{Name: "web.rp_id", Group: "Web", Kind: "string", Help: "WebAuthn relying party id: the public hostname, no scheme or port."},
	{Name: "web.origin", Group: "Web", Kind: "string", Help: "Full public origin passkeys validate against, e.g. https://host."},
	{Name: "web.autocert_domains", Group: "Web", Kind: "csv", Help: "Comma-separated hostnames autocert will request certificates for."},
	{Name: "web.tls_cert", Group: "Web", Kind: "string", Help: "PEM certificate file path for manual TLS mode."},
	{Name: "web.tls_key", Group: "Web", Kind: "string", Help: "PEM key file path for manual TLS mode."},
	{Name: "web.session_ttl", Group: "Web", Kind: "duration", Help: "How long a signed-in web session stays valid, e.g. 24h."},

	{Name: "public.enabled", Group: "Public", Kind: "bool", Help: "Enable the anonymous /public status page."},
	{Name: "public.panels", Group: "Public", Kind: "csv", Help: "Comma-separated panel ids exposed on the public page."},
	{Name: "status.title", Group: "Status page", Kind: "string", Help: "Heading of the public status page (default Status)."},
	{Name: "status.auto_resolve_after", Group: "Status page", Kind: "duration", Help: "Resolve a recovered automatic incident after this long; 0 = never (default 24h, minimum 1h)."},
	{Name: "status.echo_channels", Group: "Status page", Kind: "csv", Help: "Notification channels that receive a copy of every public status update (default none)."},

	{Name: "fleet.listen", Group: "Fleet", Kind: "string", Help: "Master only: fleet listener bind address as host:port. Default :9443.", RestartRequired: true},
	{Name: "fleet.outbox_max_mb", Group: "Fleet", Kind: "int", Help: "Child only: max MiB of telemetry spooled while the master is unreachable. Default 512.", RestartRequired: true},
	{Name: "fleet.node_down_after", Group: "Fleet", Kind: "duration", Help: "Master only: no contact for this long marks a node down and alerts. Default 2m.", RestartRequired: true},
	{Name: "fleet.fallback_after", Group: "Fleet", Kind: "duration", Help: "Child only: how long to wait for the master's delivery receipt on a routed alert before delivering it locally instead. Default 2m."},
	{Name: "fleet.link_down_warn_after", Group: "Fleet", Kind: "duration", Help: "Child only: how long the link to the master must be down before a local warning alert fires. Default 10m."},

	{Name: "server.name", Group: "Identity", Kind: "string", Help: "Display name/id for this host. Defaults to the system hostname."},

	{Name: "update.channel", Group: "Updates", Kind: "enum", Help: "Release channel for signed updates: stable, beta, or off."},
	{Name: "update.source", Group: "Updates", Kind: "enum", Help: "Where to look for releases: github, or none (bundle directories only)."},
	{Name: "update.github_token", Group: "Updates", Kind: "string", Secret: true, Help: "Read-only GitHub token, needed while the release repo is private."},
	{Name: "update.check_interval", Group: "Updates", Kind: "duration", Help: "How often to check for a newer signed release (>= 1h). Default 24h."},
}

// effectiveFastInterval returns c.FastInterval, or the default (5) for a
// zero-value Config, so sample_interval validation never divides by zero.
func (c *Config) effectiveFastInterval() int {
	if c.FastInterval <= 0 {
		return 5
	}
	return c.FastInterval
}

// effectiveSampleInterval is the slow-tier counterpart: a zero SampleInterval is
// back-filled to 60 by Load(), so fast_interval validation must check against 60.
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
