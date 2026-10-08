// Package config is the CLI-managed configuration store for trinetra.
// There is no env or hand-edited file: all keys are set via `trinetra config set`.
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
	// Name is this host's display name/id (dotted key server.name). Stored
	// empty by default so the effective name (ServerName()) tracks the live
	// system hostname; set it to disambiguate alerts from multiple hosts.
	Name string `json:"name,omitempty"`
	// SampleInterval is the slow tier: seconds between full baseline samples.
	SampleInterval int `json:"sample_interval,omitempty"`
	// FastInterval is the fast tier: seconds between lightweight checks.
	FastInterval int `json:"fast_interval,omitempty"`
	// HeartbeatInterval is the seconds between liveness heartbeats.
	HeartbeatInterval int `json:"heartbeat_interval,omitempty"`
	// ExecTimeout is the per-command hang-breaker for external collectors
	// (df/docker/systemctl/smartctl), in seconds. Deliberately generous: it
	// exists to recover from a genuinely wedged command, NOT to cap slow-but-
	// working ones, so a large/busy host does not lose collection data. Takes
	// effect on daemon (re)start.
	ExecTimeout   int     `json:"exec_timeout,omitempty"`
	BaselineSigma float64 `json:"baseline_sigma,omitempty"`
	// BaselineMinPct is the minimum relative deviation (fraction of the
	// baseline mean, e.g. 0.15 = 15%) a value must ALSO clear -- alongside
	// BaselineSigma -- before a baseline (non-threshold) anomaly fires. It
	// exists because a noisy-but-stable metric's EWMA variance can be
	// underestimated, making a small, normal wobble read as many sigma from
	// the mean; requiring the value to also be materially far from the mean
	// in relative terms suppresses that flapping. Threshold-based alerts
	// (disk/docker/etc.) are unaffected.
	BaselineMinPct float64 `json:"baseline_min_pct,omitempty"`
	// BaselineAlerts gates the baseline (z-score) deviation branch of
	// anomaly evaluation (internal/trinetra/anomaly.go breach/Evaluate):
	// threshold-based alerting (disk/docker/service/smart + cpu/mem/swap/
	// temp over their configured thresholds) is unaffected and always on.
	// Defaults to false -- field feedback showed spiky host metrics
	// (cpu/mem/temp) with a low, unstable mean firing/recovering baseline
	// alerts every minute even with BaselineSigma/BaselineMinPct's existing
	// gates, so baseline alerting is now opt-in. A plain bool (not a
	// *bool like Collect's toggles) is fine here because the desired
	// zero-value default (false) IS Go's bool zero value, so omitempty
	// dropping an unset/false value from the JSON is exactly correct --
	// unlike Collect's toggles, which default to true and so need the
	// nil-means-unset pointer trick.
	BaselineAlerts bool   `json:"baseline_alerts,omitempty"`
	QuietHours     string `json:"quiet_hours,omitempty"` // "23-8" or ""
	Telegram       struct {
		Token  string `json:"token,omitempty"`
		ChatID string `json:"chat_id,omitempty"`
		// MaxEnrollAttempts is how many consecutive wrong "/start <pin>" guesses
		// an unclaimed bot tolerates before the enrollment PIN cools down and
		// rotates (brute-force bound, #93). Unset/<=0 -> default 5. Read via
		// EnrollMaxAttempts().
		MaxEnrollAttempts int `json:"enroll_max_attempts,omitempty"`
		// EnrollCooldown is the seconds "/start" attempts are ignored after the
		// attempt threshold is hit, during which the PIN is also rotated (#93).
		// Unset/<=0 -> default 60. Read via EnrollCooldownSec().
		EnrollCooldown int `json:"enroll_cooldown,omitempty"`
	} `json:"telegram"`
	Healthchecks struct {
		URL string `json:"url,omitempty"`
	} `json:"healthchecks"`
	Notify struct {
		// BlockPrivateTargets, when true, refuses to dial loopback/link-local
		// (incl. 169.254.169.254 metadata)/private targets for the URLs the
		// daemon calls on the operator's behalf (webhook/Slack/Discord/ntfy/
		// gotify channels and the healthchecks ping). Default false, preserving
		// the ability to post to intentionally-internal endpoints; turn it on
		// to harden against SSRF via a channel URL (#97).
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
		// Backend selects the SampleStore implementation (see
		// internal/trinetra/samplestore.go and docs/handbook/09-storage-and-data-model.md).
		// One of validStorageBackends; defaults to "tsfile".
		Backend string `json:"backend,omitempty"`
		// RawRetention/RollupRetention are duration strings (time.ParseDuration
		// syntax, e.g. "48h") controlling how long the tsfile backend keeps raw
		// and 1m-rollup samples respectively (see docs/handbook/09-storage-and-data-model.md). The
		// event retention window reuses RollupRetention. Defaults: 48h / 720h.
		RawRetention    string `json:"raw_retention,omitempty"`
		RollupRetention string `json:"rollup_retention,omitempty"`
	} `json:"storage"`
	// Collect holds opt-in toggles for the expensive extended collectors
	// (docs/handbook/12-roadmap-and-status.md Epic #69). A nil pointer means "unset -> use the
	// documented default" so an explicit false survives Save/Load: a plain
	// bool with `omitempty` would drop a false value from the JSON and Load
	// would then re-fill it from Default() (true) instead of honoring it.
	Collect struct {
		// ContainerStats gates the slow-tier `docker stats` collector
		// (internal/trinetra/docker.go dockerAccess.stats). Defaults to
		// true; nil is treated as true everywhere it's read.
		ContainerStats *bool `json:"container_stats,omitempty"`
		// NetThroughput gates the slow-tier per-interface network throughput
		// collector (internal/trinetra/net.go NetRateCalc, /proc/net/dev
		// deltas -> bytes/sec). Defaults to true; nil is treated as true
		// everywhere it's read.
		NetThroughput *bool `json:"net_throughput,omitempty"`
		// Services gates the slow-tier full systemd unit inventory collector
		// (internal/trinetra/discover.go listUnits/parseUnits, snapshot
		// -only -- never persisted as a SampleStore series). Defaults to
		// true; nil is treated as true everywhere it's read. The existing
		// `systemctl --failed` alerting collection is separate and always
		// runs regardless of this setting.
		Services *bool `json:"services,omitempty"`
		// Processes gates the slow-tier process-table overview collector
		// (internal/trinetra/proc.go collectProcesses: counts + top-N by
		// CPU/mem, for the Monitoring "processes" tab). Snapshot-only -- never
		// persisted as a SampleStore series (per-process cardinality).
		// Defaults to true; nil is treated as true everywhere it's read.
		Processes *bool `json:"processes,omitempty"`
		// SmartAttrs gates the slow-tier per-device `smartctl -A` attribute
		// reads (internal/trinetra/daemon.go collectSlow, feeding the
		// "smart:<dev>:temp" series) -- the heaviest optional per-device call.
		// Defaults to true; nil is treated as true everywhere it's read. The
		// cheaper `smartctl --scan`/`-H` health checks are unaffected and
		// always run regardless of this setting.
		SmartAttrs *bool `json:"smart_attrs,omitempty"`
		// SmartInterval is the minimum seconds between SMART scans (smartctl
		// --scan/-H/-A). SMART health changes rarely and the scan is the
		// heaviest slow-tier call, so it is throttled independently of
		// sample_interval. Unset/0 -> default 1800s (30 min).
		SmartInterval int `json:"smart_interval,omitempty"`
		// PublicIP gates the host's public-IP lookup (#102), an OUTBOUND call
		// to a third-party echo service. Unlike the other collect toggles it
		// defaults to FALSE (opt-in), because it is the only one that reaches
		// off-box. nil is treated as false.
		PublicIP *bool `json:"public_ip,omitempty"`
	} `json:"collect"`
	// Web holds the web UI server's settings (internal/web, compiled into
	// the trinetra-web binary, no build tag -- see
	// docs/handbook/12-roadmap-and-status.md epic #56). The default
	// trinetra binary never reads these, but the keys live here so
	// they're manageable via `trinetra config set` regardless of which
	// binary is installed.
	Web struct {
		// Enabled toggles the web server. Defaults to false: the web UI is
		// opt-in even in the trinetra-web binary, which the core daemon
		// only supervises (spawns/restarts) when this is set.
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
	// /public status page (internal/web, compiled into the trinetra-web
	// binary, no build tag -- see docs/handbook/12-roadmap-and-status.md
	// issue #67). Both fields default to "off"/empty:
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
	// Status is the public status page (issue #157): the page title, how long
	// a recovered automatic incident waits before resolving itself ("0" =
	// never), and which notification channels receive a copy of each public
	// update.
	Status struct {
		Title            string   `json:"title,omitempty"`
		AutoResolveAfter string   `json:"auto_resolve_after,omitempty"`
		EchoChannels     []string `json:"echo_channels,omitempty"`
	} `json:"status"`
	// Fleet holds master/child fleet settings (fleet mode, see
	// docs/handbook/02-architecture.md "Fleet mode"). Role empty or "solo" means
	// no fleet code runs at all -- the default, and exactly today's behaviour.
	// Role, Address, MasterURL, CAPin and NodeID are written only by the
	// `trinetra fleet init|join|leave|disable` commands, never by
	// `config set` (Set refuses them), because they must change together with
	// the PKI files those commands create.
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

// KeepFleetIdentity copies the fleet identity keys (role, address, master
// URL, CA pin, node id) from onDisk into c. Every config save other than a
// `trinetra fleet` command goes through this so only those commands can
// change them; the tunables (listen, outbox_max_mb, node_down_after) stay
// editable.
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
// is enabled. Unlike the other collect toggles it defaults to FALSE (nil ->
// false) because it makes an outbound call.
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

// SmartIntervalSec is the effective SMART-scan throttle in seconds; unset/<=0
// defaults to 1800 (30 min). Set it as low as sample_interval to scan every slow tick.
func (c *Config) SmartIntervalSec() int {
	if c.Collect.SmartInterval <= 0 {
		return 1800
	}
	return c.Collect.SmartInterval
}

// EnrollMaxAttempts is the effective number of consecutive wrong "/start <pin>"
// guesses an unclaimed bot tolerates before the enrollment PIN cools down and
// rotates; unset/<=0 defaults to 5 (#93).
func (c *Config) EnrollMaxAttempts() int {
	if c.Telegram.MaxEnrollAttempts <= 0 {
		return 5
	}
	return c.Telegram.MaxEnrollAttempts
}

// EnrollCooldownSec is the effective number of seconds "/start" attempts are
// ignored after the attempt threshold is hit (the PIN is rotated at the same
// moment); unset/<=0 defaults to 60 (#93).
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
// declaring a node down; default 2m. An unparsable stored value (never
// written by Set, which validates) also falls back to the default.
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

// FleetFallbackAfter is how long a child waits for the master's receipt of a
// routed alert before delivering it locally instead ("via local fallback:
// master unreachable"); default 2m. An unparsable stored value (never
// written by Set, which validates) also falls back to the default.
func (c *Config) FleetFallbackAfter() time.Duration {
	if d, err := time.ParseDuration(c.Fleet.FallbackAfter); err == nil && d > 0 {
		return d
	}
	return 2 * time.Minute
}

// FleetLinkDownWarnAfter is how long a child's link to the master must be
// unreachable before it raises its own local "fleet link down" warning
// alert; default 10m. An unparsable stored value (never written by Set,
// which validates) also falls back to the default.
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

// UpdateSource is the effective self-update source: unset ("") defaults to
// "github". Set validates against github|none.
func (c *Config) UpdateSource() string {
	if c.Update.Source == "" {
		return "github"
	}
	return c.Update.Source
}

// UpdateCheckInterval is the effective interval between self-update checks:
// unset, unparsable, or below the 1h floor all default to 24h (Set never
// persists such a value, but a zero-value Config built without Default()/
// Load() must still return something sane).
func (c *Config) UpdateCheckInterval() time.Duration {
	if d, err := time.ParseDuration(c.Update.CheckInterval); err == nil && d >= time.Hour {
		return d
	}
	return 24 * time.Hour
}

// fleetManagedKeys are readable via Get but written only by the
// `trinetra fleet` commands.
var fleetManagedKeys = map[string]bool{
	"fleet.role": true, "fleet.address": true, "fleet.master_url": true,
	"fleet.ca_pin": true, "fleet.node_id": true,
}

type TargetOverride struct {
	Disabled  bool     `json:"disabled,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
}

// ChannelConfig describes one user-configured notification channel. The
// concrete delivery mechanism (Telegram, email, webhook, ...) is chosen by
// Type and is built elsewhere (package trinetra's buildNotifier factory);
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

// validSeverities is a local allowlist mirroring trinetra.Severity's
// string form. config cannot import package trinetra (that would create
// an import cycle, since trinetra imports config), so severity strings
// are validated here independently rather than via trinetra.ParseSeverity.
var validSeverities = map[string]bool{"info": true, "warning": true, "critical": true}

// validStorageBackends allowlists storage.backend. "tsfile" is the design's
// default backend (docs/handbook/09-storage-and-data-model.md, lands in a later task); "memory"
// is the in-memory reference SampleStore (internal/trinetra/samplestore.go).
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
// host/port pair -- the same "host:port" shape http.Server.Addr expects.
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
// "availability" (the public-rework task) surfaces the real 24h up/down
// strip (internal/web/availability.go's ComputeAvailability) rather than a
// scalar metric -- it's still just one more allowlist id from this package's
// point of view; internal/web decides what to render for it.
var validPublicPanels = map[string]bool{
	"availability": true,
	"cpu":          true, "mem": true, "swap": true, "load": true, "temp": true,
	"uptime": true, "services": true, "containers": true, "net": true,
}

// validatePublicPanel rejects any panel id public.panels wouldn't
// recognize: one of validPublicPanels, or "disk:<mount>" with a non-empty
// mount suffix. This is the single source of truth for what may ever be
// written to public.panels -- internal/web's /settings/public handler
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
	return fmt.Errorf("public panel %q invalid: want one of availability|cpu|mem|swap|load|temp|uptime|services|containers|net or disk:<mount>", s)
}

// parsePublicPanels parses a comma-separated public.panels value into a
// slice, trimming whitespace and dropping empty entries (mirroring
// splitKinds), validating every entry against validatePublicPanel. Returns
// the first validation error, if any -- the caller (Set) must not persist a
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
// ServerName returns the effective display name for this host: the configured
// server.name when set, else the system hostname, else "trinetra" if the
// hostname lookup fails. Resolved lazily (not baked into Default()) so the name
// tracks a renamed host instead of freezing at first run, and so Default() does
// no I/O.
func (c *Config) ServerName() string {
	if c.Name != "" {
		return c.Name
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "trinetra"
}

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

// KeyInfo describes one flat, settable config key for trinetra-ctl's
// generic "all settings" browse/edit screen (issue #91): every string this
// package's Set/Get accept, annotated with a human display Group, a short
// value-Kind hint, a one-line Help description, and whether the daemon must
// be restarted before a change takes effect. This is pure data, no new
// imports, so it does not touch cmd/trinetra's stdlib-only dependency
// graph (internal/trinetra/buildtag_test.go TestDefaultBuildIsStdlibOnly).
//
// Kind is a hint only ("int", "float", "bool", "string", "enum", "csv", or
// "duration") for how a caller should present a value before handing it to
// Set, which remains the single validated setter and source of truth for
// what is actually accepted.
type KeyInfo struct {
	Name            string
	Group           string
	Kind            string
	Help            string
	RestartRequired bool
	// Secret marks a key that holds a credential that must never be printed
	// in the clear: `config get` (full dump and single key) and any other
	// display of the config show "(set)"/"(not set)" instead. See
	// IsSecretKey.
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

// Keys returns the full catalog of flat, settable config keys, grouped for
// display. TestKeyCatalogCoversEverySetKey (config_test.go) parses (*Config)
// .Set's own switch statement and asserts this list and that switch stay in
// lockstep, so a key can never be added to one without the other going
// noticed. A copy is returned so a caller mutating the result can never
// corrupt the package-level catalog.
func Keys() []KeyInfo {
	out := make([]KeyInfo, len(keyCatalog))
	copy(out, keyCatalog)
	return out
}

// keyCatalog is Keys' backing data, grouped in the order the ctl settings
// screen presents them. RestartRequired is set for storage.* (the
// SampleStore backend is chosen once at daemon startup) and web.enabled/
// web.listen (the listener is bound once at startup), matching the
// restart caveat the guided web-setup wizard already shows for the same
// reason (setup_web.go, tui.go).
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
