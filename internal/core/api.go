package core

import (
	"context"

	"serverwatch/internal/config"
)

// API is the single boundary through which every consumer (the embedded web
// UI, the CLI, and later a socket client) reads daemon state and applies
// changes. Implementations: an in-process one inside the daemon and a
// file-backed one for the separate CLI process.
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
