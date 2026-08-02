package core

import (
	"context"

	"serverwatch/internal/config"
)

// API is the single boundary through which every consumer (the web UI, the
// CLI, and the control-socket client) reads daemon state and applies
// changes. Implementations: an in-process one inside the daemon, a
// file-backed one for CLI subcommands with no live daemon connection, and
// control.Client (internal/control), which serverwatch-ctl and
// serverwatch-web use to reach a running daemon over its control socket.
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
	// EnrollmentPIN returns the Telegram bot's current enrollment pin -- the
	// same pin the daemon's poll loop accepts via "/start <pin>" (#90) -- and
	// whether the bot is already enrolled (chat id known). pin is "" when
	// enrolled is true, or when telegram isn't configured at all.
	// Implementations without a live daemon process to ask (the file-backed
	// CLI path) return an error instead of a meaningless pin.
	EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error)
	// MonitorTargets lists every monitorable target the host currently
	// exposes (docker containers, disk mounts, network interfaces, the
	// thermal zone, smart devices -- serverwatch.Discover), independent of
	// any config.Config.Targets override: enable/disable and threshold
	// state live in Config()/ApplyConfig(), not here (mirroring how
	// MonitorTargets and TargetEnabled/TargetThreshold are already split in
	// internal/config). Unlike Monitoring(), which internal/web's
	// Monitoring page polls on every page load, this runs live discovery
	// (docker ps / df / smartctl --scan) via serverwatch.DiscoverLocal, so
	// implementations only call it when a caller (ctl's monitor-thresholds
	// screen) deliberately asks, never as part of another read's hot path.
	MonitorTargets(ctx context.Context) ([]TargetView, error)

	// writes
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
