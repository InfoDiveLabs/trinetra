package core

import (
	"context"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// API is the boundary through which the web UI, the CLI and the control-socket
// client read daemon state and apply changes. Implementations: in-process in the
// daemon, file-backed for CLI subcommands with no live daemon, and control.Client
// for reaching a running daemon over its socket.
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
	// ContainerLogs returns the last `lines` log lines of the named docker container
	// (`docker logs --tail N`). name must match a container the daemon sees; unknown
	// or malformed names are refused rather than shelled out, so callers cannot inject
	// docker arguments. Implementations without docker access return an error.
	ContainerLogs(name string, lines int) (string, error)
	// EnrollmentPIN returns the Telegram bot's current enrollment pin (accepted via
	// "/start <pin>", #90) and whether the bot is already enrolled. pin is "" when
	// enrolled or when telegram is unconfigured. Implementations with no live daemon
	// (the file-backed CLI path) return an error.
	EnrollmentPIN(ctx context.Context) (pin string, enrolled bool, err error)
	// MonitorTargets lists every monitorable target the host exposes (docker
	// containers, disk mounts, interfaces, thermal zone, smart devices;
	// trinetra.Discover), independent of any config.Config.Targets override: enable
	// and threshold state live in Config()/ApplyConfig(). It runs live discovery
	// (docker ps / df / smartctl --scan) via trinetra.DiscoverLocal, so call it only
	// on deliberate request, never in another read's hot path.
	MonitorTargets(ctx context.Context) ([]TargetView, error)
	// UpdateStatus reports this host's self-update posture from persisted state. It
	// never touches the network, so the web Updates page may call it on every render.
	UpdateStatus() (UpdateStatusView, error)

	// writes
	// UpdateCheck fetches and verifies the channel pointer and the release it names
	// (network I/O, hence ctx), records the outcome, and returns the resulting
	// UpdateStatus view. On an error (fetch/verify failure, updates off, release
	// already installed) it still returns whatever view could be built.
	UpdateCheck(ctx context.Context) (UpdateStatusView, error)
	// UpdateApply installs the given version (or the channel's latest when version is
	// ""): fetch, verify, policy-check, stage, smoke-test, swap the build in, then
	// launch the health guard, which restarts the daemon and confirms or rolls back.
	// A synchronous implementation (the CLI path) returns after the swap and guard
	// start, or when staging/verification/the swap failed. The control-socket
	// implementation runs only fast checks (settings, nothing already pending or
	// running) synchronously and continues in a background goroutine; callers poll
	// UpdateStatus's InProgress/LastError. Either way a returned error means the
	// operation never started (or was refused, e.g. a second concurrent call); a
	// failure after an async start surfaces via UpdateStatus.LastError.
	UpdateApply(ctx context.Context, version string) error
	// UpdateRollback restores the previously installed build (kept by the last
	// UpdateApply) and starts the health guard, like `trinetra update rollback`. It
	// has the same synchronous-vs-background split as UpdateApply: an error means the
	// rollback never started; later outcomes surface via UpdateStatus.
	UpdateRollback() error
	ApplyConfig(*config.Config) error
	AckAlert(key string) error
	UnackAlert(key string) error
	TestChannel(name string) error
	// ValidateChannel reports whether cc could build a working notifier, the same
	// check TestChannel/`channel test` use minus the network send. It runs against the
	// daemon's live config, not an unsaved in-flight edit, so a channel referencing
	// another field of that edit could pass or fail against stale state (accepted).
	ValidateChannel(cc config.ChannelConfig) error
	Subscribe(ctx context.Context) (<-chan Event, error)
}
