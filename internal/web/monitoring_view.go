//go:build web

package web

// MonitoringView is internal/web's own projection of the daemon's live
// Snapshot for the /monitoring detail page (ported from
// ui-mockup/monitoring.html): the Containers/Units/Processes/Filesystems
// tables, expressed with no serverwatch import — the same seam DashboardView
// (dashboard_view.go) already established for the dashboard. See that
// type's doc for why the projection crosses the internal/web <-> serverwatch
// boundary this way instead of serverwatch.Snapshot itself, and for the
// map-safety/copy-only contract internal/serverwatch/daemon_web.go's
// buildMonitoringView adapter must honor when building one of these.
type MonitoringView struct {
	// Containers is every container the daemon's plain state listing knows
	// about (serverwatch.Snapshot.Containers, name -> state), each merged
	// with its live docker-stats row (ContainerStats) when present — see
	// MonitoringContainerView.HasStats for when cpu/mem/net are meaningful.
	Containers []MonitoringContainerView `json:"containers,omitempty"`

	// FailedUnits is the systemd units currently in a failed state
	// (serverwatch.Snapshot.FailedUnits) — always populated regardless of
	// UnitsEnabled below, since `systemctl --failed` is always collected
	// (the same alerting input service:* checks use), independent of the
	// opt-in full-inventory collector.
	FailedUnits []string `json:"failed_units,omitempty"`
	// Units is the full systemd unit inventory (serverwatch.Snapshot.Units),
	// populated only when UnitsEnabled — the page renders a "collector
	// disabled" note in its place otherwise, rather than an empty table
	// that looks like "zero units" (which is never really true).
	Units []MonitoringUnitView `json:"units,omitempty"`
	// UnitsEnabled mirrors config.Config.ServicesEnabled() (collect.services)
	// at adapt time.
	UnitsEnabled bool `json:"units_enabled"`

	// Processes is the top-N process list (serverwatch.Snapshot.Processes.Top),
	// populated only when ProcessesEnabled — same "collector disabled" note
	// otherwise.
	Processes []MonitoringProcessView `json:"processes,omitempty"`
	// ProcessesEnabled mirrors config.Config.ProcessesEnabled()
	// (collect.processes) at adapt time.
	ProcessesEnabled bool `json:"processes_enabled"`
	// ProcessesTotal is the full process-table count
	// (serverwatch.Snapshot.Processes.Total) backing the "Top by CPU · N
	// total" note beneath the processes table; 0 whenever ProcessesEnabled
	// is false (there is no total to report).
	ProcessesTotal int `json:"processes_total"`

	// Disks is every mounted filesystem (serverwatch.Snapshot.Disks) merged
	// with its DiskDetail (device/fstype/inode%/size/free/fill projection)
	// when present — always collected, no collector toggle, mirroring
	// DashboardView.Disks.
	Disks []MonitoringDiskView `json:"disks,omitempty"`
}

// MonitoringContainerView is one container row: name/state always present;
// cpu%/mem/net are only meaningful when HasStats is true (collect.container_stats
// enabled AND docker stats actually reported this container this tick).
type MonitoringContainerView struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	CPUPct   float64 `json:"cpu_pct,omitempty"`
	MemMiB   float64 `json:"mem_mib,omitempty"`
	NetRxMB  float64 `json:"net_rx_mb,omitempty"`
	NetTxMB  float64 `json:"net_tx_mb,omitempty"`
	HasStats bool    `json:"has_stats"`
}

// MonitoringUnitView is one systemd unit row from the full inventory
// (only populated when MonitoringView.UnitsEnabled).
type MonitoringUnitView struct {
	Name        string `json:"name"`
	Load        string `json:"load"`
	Active      string `json:"active"`
	Sub         string `json:"sub"`
	Description string `json:"description"`
}

// MonitoringProcessView is one top-N process row (only populated when
// MonitoringView.ProcessesEnabled).
type MonitoringProcessView struct {
	PID     int     `json:"pid"`
	Name    string  `json:"name"`
	State   string  `json:"state"`
	CPUPct  float64 `json:"cpu_pct"`
	MemMiB  float64 `json:"mem_mib"`
	Threads int     `json:"threads"`
}

// MonitoringDiskView is one mounted filesystem's full detail — a superset of
// DashboardView's DiskView (adds FSType/InodePct, which the dashboard's
// compact filesystems table doesn't show but the Monitoring page's does).
type MonitoringDiskView struct {
	Mount     string  `json:"mount"`
	Device    string  `json:"device"`
	FSType    string  `json:"fs_type"`
	UsagePct  float64 `json:"usage_pct"`
	InodePct  float64 `json:"inode_pct"`
	FreeBytes uint64  `json:"free_bytes"`
	SizeBytes uint64  `json:"size_bytes"`
	// DaysToFull/DaysToFullKnown mirror DiskView's identically named fields
	// (serverwatch.DiskDetail's linear-fill projection).
	DaysToFull      float64 `json:"days_to_full,omitempty"`
	DaysToFullKnown bool    `json:"days_to_full_known,omitempty"`
}
