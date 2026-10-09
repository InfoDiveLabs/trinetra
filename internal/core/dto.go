package core

// Resolution selects the granularity a Series query is served at: raw
// per-sample points, or the coarser 1-minute rollups.
type Resolution int

const (
	ResRaw Resolution = iota
	Res1m
	// ResAuto lets the implementation pick the resolution, via trinetra.PickResolution.
	ResAuto
)

// SeriesPoint is one time-series sample: TS is Unix seconds, Min/Avg/Max are the same value
// for a raw point, or a real rollup for a downsampled one.
type SeriesPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// DownEventView is one downtime event as rendered to a consumer: Type says what went down
// (e.g. "net_down"), Start/End are Unix seconds (End is 0 while open).
type DownEventView struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}

// DashboardView is core's projection of the daemon's live Snapshot: the fields the
// dashboard template and the /events SSE stream need, with no trinetra import.
type DashboardView struct {
	// TS is the Unix-seconds timestamp the sampler stamped on the snapshot; it shows
	// how stale the view is.
	TS int64 `json:"ts"`
	// Online is the last-observed internet-reachability check.
	Online bool `json:"online"`

	// CPU/MemPct/SwapPct/Load1/5/15/TempC mirror Snapshot's identically
	// named fields; TempC is 0 when no thermal sensor was found.
	CPU     float64 `json:"cpu"`
	MemPct  float64 `json:"mem_pct"`
	SwapPct float64 `json:"swap_pct"`
	Load1   float64 `json:"load1"`
	Load5   float64 `json:"load5"`
	Load15  float64 `json:"load15"`
	TempC   float64 `json:"temp_c"`
	// Cores is runtime.NumCPU() on the daemon's host, filled by the adapter, for the
	// CPU tile subtext.
	Cores int `json:"cores"`

	// Processes is the live process-table overview (Snapshot.Processes),
	// zero-valued whenever collect.processes is disabled.
	Processes ProcessCounts `json:"processes"`

	// ContainersRunning/ContainersTotal summarize Snapshot.Containers (name
	// -> state) for the dashboard's summary count tile.
	ContainersRunning int `json:"containers_running"`
	ContainersTotal   int `json:"containers_total"`
	// TopCPUContainers/TopMemContainers are Snapshot.ContainerStats sorted desc by CPU%/MemMiB
	// and capped to dashboardTopN.
	TopCPUContainers []ContainerView `json:"top_cpu_containers,omitempty"`
	TopMemContainers []ContainerView `json:"top_mem_containers,omitempty"`

	// UnitsFailed/UnitsTotal summarize Snapshot.FailedUnits (always collected) and
	// Snapshot.Units (only with collect.services; UnitsTotal is 0 otherwise).
	UnitsFailed int `json:"units_failed"`
	UnitsTotal  int `json:"units_total"`

	// Disks is Snapshot.Disks (+ DiskDetail where present) merged into one
	// row per mount, sorted by mount path, for the filesystems table.
	Disks []DiskView `json:"disks,omitempty"`
	// DisksCritical is how many of Disks are at/above DiskCriticalPct, for
	// the summary count tile.
	DisksCritical int `json:"disks_critical"`

	// NetIfaces is Snapshot.NetRates sorted by interface name; NetRxBps/ NetTxBps are its
	// sums, for the network tile/chart.
	NetIfaces []NetIfaceView `json:"net_ifaces,omitempty"`
	NetRxBps  float64        `json:"net_rx_bps"`
	NetTxBps  float64        `json:"net_tx_bps"`

	// Availability is the dashboard's 24h up/down strip (ComputeAvailability).
	Availability Availability `json:"availability"`

	// DegradedCollectors lists slow-tier collectors that are failing (#110), with
	// consecutive-failure count and last error.
	DegradedCollectors []CollectorHealthView `json:"degraded_collectors,omitempty"`
}

// CollectorHealthView is one degraded slow-tier collector (#110), the DTO form of
// trinetra.CollectorStat.
type CollectorHealthView struct {
	Name            string `json:"name"`              // "docker" | "disk" | "services" | "smart"
	Fails           int    `json:"fails"`             // consecutive failed cycles
	LastError       string `json:"last_error"`        // most recent error text
	LastSuccessUnix int64  `json:"last_success_unix"` // 0 if never succeeded
}

// ProcessCounts mirrors trinetra.ProcSnapshot's aggregate counts; Top is omitted
// because the dashboard only shows counts.
type ProcessCounts struct {
	Total    int `json:"total"`
	Running  int `json:"running"`
	Sleeping int `json:"sleeping"`
	Zombie   int `json:"zombie"`
}

// ContainerView is one docker container's name/state plus (if
// collect.container_stats is enabled) its live resource usage.
type ContainerView struct {
	Name   string  `json:"name"`
	State  string  `json:"state"`
	CPUPct float64 `json:"cpu_pct"`
	MemMiB float64 `json:"mem_mib"`
}

// DiskView is one mounted filesystem's usage plus whatever DiskDetail the
// slow-tier collector found for it (device/size/free/fill projection).
type DiskView struct {
	Mount     string  `json:"mount"`
	Device    string  `json:"device"`
	UsagePct  float64 `json:"usage_pct"`
	FreeBytes uint64  `json:"free_bytes"`
	SizeBytes uint64  `json:"size_bytes"`
	// DaysToFull/DaysToFullKnown mirror trinetra.DiskDetail's linear-fill projection;
	// DaysToFullKnown false means no meaningful trend yet.
	DaysToFull      float64 `json:"days_to_full,omitempty"`
	DaysToFullKnown bool    `json:"days_to_full_known,omitempty"`
}

// NetIfaceView is one network interface's live throughput.
type NetIfaceView struct {
	Name  string  `json:"name"`
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
}

// dashboardTopN bounds TopCPUContainers/TopMemContainers.
const dashboardTopN = 4

// DiskCriticalPct is the usage percentage at/above which a mount counts toward
// DashboardView.DisksCritical and renders as "crit".
const DiskCriticalPct = 90.0

// DiskWarnPct is the warn-level counterpart to DiskCriticalPct, also display-only.
const DiskWarnPct = 70.0

// MonitoringView is core's projection of the live Snapshot for the /monitoring page
// (Containers/Units/Processes/Filesystems tables), with no trinetra import.
type MonitoringView struct {
	// Containers is every container in the daemon's state listing, each merged with
	// its docker-stats row when present (see MonitoringContainerView.HasStats).
	Containers []MonitoringContainerView `json:"containers,omitempty"`

	// FailedUnits is the systemd units in a failed state.
	FailedUnits []string `json:"failed_units,omitempty"`
	// Units is the full systemd unit inventory, populated only when UnitsEnabled; the
	// page shows a "collector disabled" note otherwise rather than an empty table.
	Units []MonitoringUnitView `json:"units,omitempty"`
	// UnitsEnabled mirrors config.Config.ServicesEnabled() (collect.services).
	UnitsEnabled bool `json:"units_enabled"`

	// Processes is the top-N process list, populated only when ProcessesEnabled.
	Processes []MonitoringProcessView `json:"processes,omitempty"`
	// ProcessesEnabled mirrors config.Config.ProcessesEnabled() (collect.processes).
	ProcessesEnabled bool `json:"processes_enabled"`
	// ProcessesTotal is the full process-table count behind the "Top by CPU · N
	// total" note; 0 when ProcessesEnabled is false.
	ProcessesTotal int `json:"processes_total"`

	// Disks is every mounted filesystem merged with its DiskDetail when present.
	Disks []MonitoringDiskView `json:"disks,omitempty"`
}

// MonitoringContainerView is one container row. cpu%/mem/net are meaningful only when
// HasStats is true.
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

// MonitoringDiskView is one mounted filesystem's full detail, a superset of
// DiskView adding FSType/InodePct.
type MonitoringDiskView struct {
	Mount     string  `json:"mount"`
	Device    string  `json:"device"`
	FSType    string  `json:"fs_type"`
	UsagePct  float64 `json:"usage_pct"`
	InodePct  float64 `json:"inode_pct"`
	FreeBytes uint64  `json:"free_bytes"`
	SizeBytes uint64  `json:"size_bytes"`
	// DaysToFull/DaysToFullKnown mirror DiskView's fields.
	DaysToFull      float64 `json:"days_to_full,omitempty"`
	DaysToFullKnown bool    `json:"days_to_full_known,omitempty"`
}

// DisksWarnCritCount is how many of v.Disks are at/above DiskWarnPct, shared with
// internal/web's monitoringStatus.
func (v MonitoringView) DisksWarnCritCount() int {
	n := 0
	for _, d := range v.Disks {
		if d.UsagePct >= DiskWarnPct {
			n++
		}
	}
	return n
}

// TargetView is one monitorable target as core.API.MonitorTargets reports it, mirroring
// trinetra.Target's exported fields so core need not import internal/trinetra.
type TargetView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Display   string `json:"display"`
	Available bool   `json:"available"`
}

// AlertRecord is one alert as rendered to a consumer, covering both an active alert
// (trinetra.ActiveAlert, keyed by Key) and a historical fire/recover entry.
type AlertRecord struct {
	Key      string `json:"key"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Time     int64  `json:"time"`
	Acked    bool   `json:"acked"`
	// AckedAt is the Unix-seconds a manual ack was recorded; 0 for a history entry.
	AckedAt int64 `json:"acked_at,omitempty"`
	// Title is the alert-log AlertEvent's Title; empty for an active alert.
	Title string `json:"title,omitempty"`
	// Delivered is true when a history entry's dispatch reached at least one channel (a
	// Delivery record with OK true); false if every attempt failed, none was recorded.
	Delivered bool `json:"delivered,omitempty"`
	// DeliveredTo names the channels that accepted a history entry's dispatch, in record
	// order, so a consumer can show which channels were reached.
	DeliveredTo []string `json:"delivered_to,omitempty"`
}

// DoctorReport is core's projection of `trinetra doctor`'s output (cmdDoctor): docker
// reachability, smartctl availability, discovered thermal zones and targets.
type DoctorReport struct {
	// DockerAccess mirrors cmdDoctor's "docker: available=%v method=%s" line.
	DockerAccess      string `json:"docker_access"`
	SmartctlAvailable bool   `json:"smartctl_available"`
	ThermalZones      int    `json:"thermal_zones"`
	TargetsDiscovered int    `json:"targets_discovered"`

	// ContainerStatsOn/NetThroughputOn/ServicesOn/ProcessesOn/SmartAttrsOn mirror
	// the config.Config *Enabled() methods, one per opt-in collector.
	ContainerStatsOn bool `json:"container_stats_on"`
	NetThroughputOn  bool `json:"net_throughput_on"`
	ServicesOn       bool `json:"services_on"`
	ProcessesOn      bool `json:"processes_on"`
	SmartAttrsOn     bool `json:"smart_attrs_on"`

	// StoreStats mirrors collectorSummary's "time-series: N series, X.X MB on disk
	// (raw+1m)" line, or "time-series: unavailable" when the store failed to open.
	StoreStats string `json:"store_stats"`
	// StoreWarning is a non-empty guardrail message when the series count is abnormally high
	// (#112), e.g. dead Swarm-task series accumulating faster than retention reaps them.
	StoreWarning string `json:"store_warning,omitempty"`
}

// Event is one live daemon event pushed to a core.API.Subscribe stream.
type Event struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Time     int64  `json:"time"`
}

// HostInfoView is the static host hardware/OS inventory (#100): RAM, CPU model and
// core/thread split, kernel and OS, per-disk hardware.
type HostInfoView struct {
	Hostname      string         `json:"hostname"`
	Kernel        string         `json:"kernel"`
	OS            string         `json:"os"`
	CPUModel      string         `json:"cpu_model"`
	CPUSockets    int            `json:"cpu_sockets,omitempty"` // physical packages
	CPUCores      int            `json:"cpu_cores"`             // physical
	CPUThreads    int            `json:"cpu_threads"`           // logical
	CPUBaseMHz    float64        `json:"cpu_base_mhz,omitempty"`
	MemTotalBytes uint64         `json:"mem_total_bytes"`
	BootTime      int64          `json:"boot_time"`
	UptimeSec     int64          `json:"uptime_sec"`
	LocalIP       string         `json:"local_ip,omitempty"`
	PublicIP      string         `json:"public_ip,omitempty"`
	Disks         []HostDiskView `json:"disks,omitempty"`
}

// UpdateStatusView is core's projection of a host's self-update posture.
type UpdateStatusView struct {
	Running    string             `json:"running"`
	Channel    string             `json:"channel"`
	Floor      string             `json:"floor"`
	Available  string             `json:"available"`
	Previous   string             `json:"previous"`
	Source     string             `json:"source"`
	Pending    *UpdatePendingView `json:"pending,omitempty"`
	Last       *UpdateResultView  `json:"last,omitempty"`
	KeysLoaded bool               `json:"keys_loaded"`
	LastCheck  int64              `json:"last_check,omitempty"` // unix seconds of the last successful channel check; 0 = never
	// LastCheckError is why the latest channel check failed to verify a pointer, cleared once
	// one verifies.
	LastCheckError string `json:"last_check_error,omitempty"`
	InProgress     bool   `json:"in_progress"`
	LastError      string `json:"last_error,omitempty"`
}

// UpdatePendingView is core's projection of update.Pending: an update staged and awaiting
// its health-guard deadline.
type UpdatePendingView struct {
	Version  string `json:"version"`
	From     string `json:"from"`
	Deadline int64  `json:"deadline"`
	Rollback bool   `json:"rollback"`
	// RestoreFailed mirrors update.Pending.RestoreFailed: set once the guard's health-gate
	// rollback itself failed to restore the previous build, so this Pending is being kept.
	RestoreFailed string `json:"restore_failed,omitempty"`
}

// UpdateResultView is core's projection of update.Result: the outcome of the most recently
// confirmed or rolled-back update attempt.
type UpdateResultView struct {
	Version string `json:"version"`
	From    string `json:"from"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	At      int64  `json:"at"`
}

// HostDiskView is one physical/block disk backing a mounted filesystem.
type HostDiskView struct {
	Device     string `json:"device"`
	Model      string `json:"model,omitempty"`
	Rotational bool   `json:"rotational"`
	SizeBytes  uint64 `json:"size_bytes"`
	FSType     string `json:"fstype,omitempty"`
	Mount      string `json:"mount"`
}
