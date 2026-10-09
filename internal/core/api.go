package core

import (
	"context"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// API is the boundary through which the web UI, the CLI and the control-socket client read
// daemon state and apply changes.
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
	// HostInfo returns the static host hardware/OS inventory (#100), fetched once
	// rather than per tick.
	HostInfo() (HostInfoView, error)
	// Version returns the core daemon's build-stamped version (#107), e.g.
	// "v0.4.1-beta.3" or "dev", so a plugin can detect version drift.
	Version() (string, error)
	// ContainerLogs returns the last `lines` log lines of the named docker container (`docker
	// logs --tail N`). name must match a container the daemon sees.
	ContainerLogs(name string, lines int) (string, error)
	// EnrollmentPIN returns the Telegram bot's current enrollment pin.
	EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error)
	// MonitorTargets lists every monitorable target the host exposes (docker containers, disk
	// mounts, interfaces, thermal zone, smart devices; trinetra.Discover).
	MonitorTargets(ctx context.Context) ([]TargetView, error)
	// UpdateStatus reports this host's self-update posture from persisted state.
	UpdateStatus() (UpdateStatusView, error)

	// writes UpdateCheck fetches and verifies the channel pointer and the release it names
	// (network I/O, hence ctx), records the outcome.
	UpdateCheck(ctx context.Context) (UpdateStatusView, error)
	// UpdateApply installs the given version (or the channel's latest when version is ""):
	// fetch, verify, policy-check, stage, smoke-test.
	UpdateApply(ctx context.Context, version string) error
	// UpdateRollback restores the previously installed build (kept by the last UpdateApply)
	// and starts the health guard, like `trinetra update rollback`.
	UpdateRollback() error
	ApplyConfig(*config.Config) error
	AckAlert(key string) error
	UnackAlert(key string) error
	TestChannel(name string) error
	// ValidateChannel reports whether cc could build a working notifier, the same check
	// TestChannel/`channel test` use minus the network send.
	ValidateChannel(cc config.ChannelConfig) error
	Subscribe(ctx context.Context) (<-chan Event, error)
}
