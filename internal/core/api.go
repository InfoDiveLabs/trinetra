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
	Subscribe(ctx context.Context) (<-chan Event, error)
}
