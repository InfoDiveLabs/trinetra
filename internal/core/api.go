package core

import (
	"context"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// API is the single boundary through which every consumer (the web UI, the
// CLI, and the control-socket client) reads daemon state and applies
// changes. Implementations: an in-process one inside the daemon, a
// file-backed one for CLI subcommands with no live daemon connection, and
// control.Client (internal/control), which trinetra-ctl and
// trinetra-web use to reach a running daemon over its control socket.
type API interface {
	// reads
	Snapshot() (DashboardView, error)
	Monitoring() (MonitoringView, error)
	Series(metric string, from, to int64, res Resolution) ([]SeriesPoint, error)
	Events(from, to int64) ([]DownEventView, error)
	ActiveAlerts() ([]AlertRecord, error)
	AlertHistory(since int64, limit int) ([]AlertRecord, error)
	Config() (*config.Config, error)
	Doctor() (DoctorReport, error)
	// HostInfo returns the static host hardware/OS inventory (#100): RAM, CPU,
	// kernel/OS, per-disk hardware, plus boot time and derived uptime. Static
	// for a boot, so callers fetch it once rather than per tick.
	HostInfo() (HostInfoView, error)
	// Version returns the core daemon's build-stamped product version (#107),
	// e.g. "v0.4.1-beta.3" or "dev". A plugin dials this over the socket to
	// show the running core version and to detect a core/plugin version drift
	// against its own compiled-in version.
	Version() (string, error)
	// ContainerLogs returns the last `lines` log lines of the named docker
	// container (a `docker logs --tail N` snapshot, newest at the bottom).
	// name must match a container the daemon currently sees; an unknown or
	// malformed name is refused rather than shelled out, so a caller cannot use
	// this to run arbitrary docker arguments. Implementations without docker
	// access return an error.
	ContainerLogs(name string, lines int) (string, error)
	// EnrollmentPIN returns the Telegram bot's current enrollment pin -- the
	// same pin the daemon's poll loop accepts via "/start <pin>" (#90) -- and
	// whether the bot is already enrolled (chat id known). pin is "" when
	// enrolled is true, or when telegram isn't configured at all.
	// Implementations without a live daemon process to ask (the file-backed
	// CLI path) return an error instead of a meaningless pin.
	EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error)
	// MonitorTargets lists every monitorable target the host currently
	// exposes (docker containers, disk mounts, network interfaces, the
	// thermal zone, smart devices -- trinetra.Discover), independent of
	// any config.Config.Targets override: enable/disable and threshold
	// state live in Config()/ApplyConfig(), not here (mirroring how
	// MonitorTargets and TargetEnabled/TargetThreshold are already split in
	// internal/config). Unlike Monitoring(), which internal/web's
	// Monitoring page polls on every page load, this runs live discovery
	// (docker ps / df / smartctl --scan) via trinetra.DiscoverLocal, so
	// implementations only call it when a caller (ctl's monitor-thresholds
	// screen) deliberately asks, never as part of another read's hot path.
	MonitorTargets(ctx context.Context) ([]TargetView, error)
	// UpdateStatus reports this host's current self-update posture (channel,
	// floor, available/previous versions, any pending update, the last
	// apply/rollback outcome, and whether release keys are compiled in) from
	// persisted state -- it never fetches over the network, so it is cheap
	// enough for the web Updates page to call on every render.
	UpdateStatus() (UpdateStatusView, error)

	// writes
	// UpdateCheck fetches and verifies the channel pointer and the release it
	// names (network I/O, hence ctx), records the outcome in persisted state,
	// and returns the resulting UpdateStatus view. An error here (a fetch/
	// verify failure, updates being off, or the release already being
	// installed) still returns whatever status view could be built.
	UpdateCheck(ctx context.Context) (UpdateStatusView, error)
	// UpdateApply installs the given version (or, when version is "", the
	// channel's latest) synchronously: fetch, verify, policy-check, stage,
	// smoke-test, and swap the build in, then launch the health guard. It
	// returns once the swap has happened and the guard has been asked to
	// start (or once staging/verification/the swap itself failed) -- the
	// guard, not this call, is what restarts the daemon and confirms or
	// rolls back the new build.
	UpdateApply(ctx context.Context, version string) error
	// UpdateRollback restores the previously installed build (kept by the
	// last successful UpdateApply) and starts the health guard to confirm it,
	// mirroring `trinetra update rollback`.
	UpdateRollback() error
	ApplyConfig(*config.Config) error
	AckAlert(key string) error
	UnackAlert(key string) error
	TestChannel(name string) error
	// ValidateChannel reports whether cc could actually build a working
	// notifier -- the same check TestChannel/`channel test` deliver against,
	// minus the network send. It is checked against the daemon's current
	// live config, not any unsaved in-flight edit a caller may be building
	// cc as part of: a channel referencing another field of that in-flight
	// edit (rare in practice) could pass or fail this check against stale
	// state. That is an accepted limitation, not a bug.
	ValidateChannel(cc config.ChannelConfig) error
	Subscribe(ctx context.Context) (<-chan Event, error)
}
