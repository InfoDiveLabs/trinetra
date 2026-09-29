package core

// Resolution selects the granularity a Series query is served at: raw
// per-sample points, or the coarser 1-minute rollups.
type Resolution int

const (
	ResRaw Resolution = iota
	Res1m
	// ResAuto is a sentinel meaning "let the implementation pick the
	// resolution". A Series caller that doesn't need to care about raw vs
	// 1m can pass this; a later task's implementation resolves it via
	// trinetra.PickResolution (the age/config-dependent picker, which
	// stays in internal/trinetra since it needs the store's actual
	// raw-retention configuration, out of reach for this stdlib-only
	// package).
	ResAuto
)

// SeriesPoint is one time-series sample: TS is Unix seconds, Min/Avg/Max
// are the same value for a raw point, or a real rollup for a downsampled
// one.
type SeriesPoint struct {
	TS  int64   `json:"ts"`
	Min float64 `json:"min"`
	Avg float64 `json:"avg"`
	Max float64 `json:"max"`
}

// DownEventView is one downtime event (a target or interface going down
// and, if it has ended, coming back), as rendered to a consumer: Type
// identifies what went down (for example "net_down"), Start/End are Unix
// seconds (End is 0 while the event is still open), and DurationSec is the
// event's length in seconds.
type DownEventView struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}

// DashboardView is core's projection of the daemon's live Snapshot: exactly
// the fields templates/dashboard.html (and the /events SSE stream, sse.go)
// need to render the dashboard, expressed with no trinetra import.
//
// Deps.Snapshot returns this type (aliased in internal/web as
// web.DashboardView) rather than trinetra.Snapshot itself because this
// package must never import internal/trinetra (see doc.go's import
// contract). internal/trinetra/coreapi_inproc.go's buildDashboardView
// builds one of these by copying values out of a trinetra.Snapshot; the
// trinetra-web binary's buildDeps (cmd/trinetra-web) carries the
// finished value across the control socket into Deps, so web only ever
// consumes it via the alias, never importing trinetra directly.
//
// Every field here is a copy (scalars, or a freshly built slice) taken from
// the Snapshot at adapt time, never a map/slice alias into it: the adapter
// only reads the Snapshot it's given, exactly as the concurrency contract in
// internal/trinetra/snapshot_hub.go's snapshotHub doc requires (readers
// must never mutate a published Snapshot's map fields -- see that file for
// why that invariant is what makes the atomic.Pointer safe without a lock).
type DashboardView struct {
	// TS is the Unix-seconds timestamp the daemon's sampler loop stamped
	// onto this snapshot (Snapshot.TS) -- how stale the view is.
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
	// Cores is runtime.NumCPU() on the daemon's own host (the adapter fills
	// this in -- it isn't part of Snapshot itself), for the CPU tile's "N
	// cores" subtext.
	Cores int `json:"cores"`

	// Processes is the live process-table overview (Snapshot.Processes),
	// zero-valued whenever collect.processes is disabled.
	Processes ProcessCounts `json:"processes"`

	// ContainersRunning/ContainersTotal summarize Snapshot.Containers (name
	// -> state) for the dashboard's summary count tile.
	ContainersRunning int `json:"containers_running"`
	ContainersTotal   int `json:"containers_total"`
	// TopCPUContainers/TopMemContainers are Snapshot.ContainerStats sorted
	// desc by CPU%/MemMiB respectively and capped to dashboardTopN, for the
	// "Top containers" hbar panels. Empty whenever collect.container_stats
	// is disabled (ContainerStats is nil/empty on the Snapshot).
	TopCPUContainers []ContainerView `json:"top_cpu_containers,omitempty"`
	TopMemContainers []ContainerView `json:"top_mem_containers,omitempty"`

	// UnitsFailed/UnitsTotal summarize Snapshot.FailedUnits (always
	// collected) and Snapshot.Units (only populated when collect.services is
	// enabled -- UnitsTotal is 0 otherwise even if UnitsFailed isn't).
	UnitsFailed int `json:"units_failed"`
	UnitsTotal  int `json:"units_total"`

	// Disks is Snapshot.Disks (+ DiskDetail where present) merged into one
	// row per mount, sorted by mount path, for the filesystems table.
	Disks []DiskView `json:"disks,omitempty"`
	// DisksCritical is how many of Disks are at/above DiskCriticalPct, for
	// the summary count tile.
	DisksCritical int `json:"disks_critical"`

	// NetIfaces is Snapshot.NetRates sorted by interface name; NetRxBps/
	// NetTxBps are its sums, for the network tile/chart. Empty/zero whenever
	// collect.net_throughput is disabled or hasn't produced a rate yet.
	NetIfaces []NetIfaceView `json:"net_ifaces,omitempty"`
	NetRxBps  float64        `json:"net_rx_bps"`
	NetTxBps  float64        `json:"net_tx_bps"`

	// Availability is the dashboard's real 24h up/down strip data
	// (availability.go's ComputeAvailability), replacing the old hardcoded
	// #hbstrip demo in app.js. Zero-valued (no blocks) whenever the caller
	// building this view doesn't have a downtime EventsSource to compute it
	// from (e.g. a bare `DashboardView{}` in a test that doesn't care about
	// the strip).
	Availability Availability `json:"availability"`

	// DegradedCollectors lists slow-tier collectors that are currently failing
	// (#110): each carries its consecutive-failure count and last error so an
	// operator sees that MONITORING ITSELF is degraded, not just missing data.
	// Empty when every collector is healthy. Shown as a warning banner in the
	// web dashboard and a status line in trinetra-ctl.
	DegradedCollectors []CollectorHealthView `json:"degraded_collectors,omitempty"`
}

// CollectorHealthView is one degraded slow-tier collector's health (#110), the
// core-DTO projection of trinetra.CollectorStat.
type CollectorHealthView struct {
	Name            string `json:"name"`              // "docker" | "disk" | "services" | "smart"
	Fails           int    `json:"fails"`             // consecutive failed cycles
	LastError       string `json:"last_error"`        // most recent error text
	LastSuccessUnix int64  `json:"last_success_unix"` // 0 if never succeeded
}

// ProcessCounts mirrors trinetra.ProcSnapshot's aggregate counts (Top is
// deliberately not carried here -- the dashboard only shows counts, not a
// process list; that's the Monitoring page's job).
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
	// DaysToFull/DaysToFullKnown mirror trinetra.DiskDetail's linear-fill
	// projection; DaysToFullKnown false means "no meaningful trend yet"
	// (fewer than 2 history points, or flat/declining usage).
	DaysToFull      float64 `json:"days_to_full,omitempty"`
	DaysToFullKnown bool    `json:"days_to_full_known,omitempty"`
}

// NetIfaceView is one network interface's live throughput.
type NetIfaceView struct {
	Name  string  `json:"name"`
	RxBps float64 `json:"rx_bps"`
	TxBps float64 `json:"tx_bps"`
}

// dashboardTopN bounds TopCPUContainers/TopMemContainers, mirroring the
// mockup dashboard.html's 4-row "Top containers" hbar panels.
const dashboardTopN = 4

// DiskCriticalPct is the usage percentage at/above which a mount counts
// toward DashboardView.DisksCritical and renders with the "crit" meter/led
// color -- a display-only threshold for the dashboard's summary tiles,
// independent of (and not a substitute for) the daemon's own configurable
// alert thresholds (internal/config), which keep driving real alert
// delivery. Exported so internal/trinetra/coreapi_inproc.go's
// buildDashboardView adapter can count DisksCritical using the exact same
// cutoff this package's template uses to color the same mounts.
const DiskCriticalPct = 90.0

// DiskWarnPct is the warn-level counterpart to DiskCriticalPct, same
// display-only caveat.
const DiskWarnPct = 70.0

// MonitoringView is core's projection of the daemon's live Snapshot for the
// /monitoring detail page (ported from ui-mockup/monitoring.html): the
// Containers/Units/Processes/Filesystems tables, expressed with no
// trinetra import -- the same seam DashboardView already established for
// the dashboard. See that type's doc for why the projection crosses the
// core <-> trinetra boundary this way instead of trinetra.Snapshot
// itself, and for the map-safety/copy-only contract
// internal/trinetra/coreapi_inproc.go's buildMonitoringView adapter must
// honor when building one of these.
type MonitoringView struct {
	// Containers is every container the daemon's plain state listing knows
	// about (trinetra.Snapshot.Containers, name -> state), each merged
	// with its live docker-stats row (ContainerStats) when present -- see
	// MonitoringContainerView.HasStats for when cpu/mem/net are meaningful.
	Containers []MonitoringContainerView `json:"containers,omitempty"`

	// FailedUnits is the systemd units currently in a failed state
	// (trinetra.Snapshot.FailedUnits) -- always populated regardless of
	// UnitsEnabled below, since `systemctl --failed` is always collected
	// (the same alerting input service:* checks use), independent of the
	// opt-in full-inventory collector.
	FailedUnits []string `json:"failed_units,omitempty"`
	// Units is the full systemd unit inventory (trinetra.Snapshot.Units),
	// populated only when UnitsEnabled -- the page renders a "collector
	// disabled" note in its place otherwise, rather than an empty table
	// that looks like "zero units" (which is never really true).
	Units []MonitoringUnitView `json:"units,omitempty"`
	// UnitsEnabled mirrors config.Config.ServicesEnabled() (collect.services)
	// at adapt time.
	UnitsEnabled bool `json:"units_enabled"`

	// Processes is the top-N process list (trinetra.Snapshot.Processes.Top),
	// populated only when ProcessesEnabled -- same "collector disabled" note
	// otherwise.
	Processes []MonitoringProcessView `json:"processes,omitempty"`
	// ProcessesEnabled mirrors config.Config.ProcessesEnabled()
	// (collect.processes) at adapt time.
	ProcessesEnabled bool `json:"processes_enabled"`
	// ProcessesTotal is the full process-table count
	// (trinetra.Snapshot.Processes.Total) backing the "Top by CPU · N
	// total" note beneath the processes table; 0 whenever ProcessesEnabled
	// is false (there is no total to report).
	ProcessesTotal int `json:"processes_total"`

	// Disks is every mounted filesystem (trinetra.Snapshot.Disks) merged
	// with its DiskDetail (device/fstype/inode%/size/free/fill projection)
	// when present -- always collected, no collector toggle, mirroring
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

// MonitoringDiskView is one mounted filesystem's full detail -- a superset
// of DashboardView's DiskView (adds FSType/InodePct, which the dashboard's
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
	// (trinetra.DiskDetail's linear-fill projection).
	DaysToFull      float64 `json:"days_to_full,omitempty"`
	DaysToFullKnown bool    `json:"days_to_full_known,omitempty"`
}

// DisksWarnCritCount is how many of v.Disks are at/above DiskWarnPct --
// exported so internal/web's monitoringStatus (and, if useful later, the
// template itself) can share one definition of "warn or worse" with
// buildMonitoringPageData's DisksWarnCrit field. Defined here (rather than
// in internal/web, where it lived before this type moved into core) because
// Go methods can only be declared on a type in its own package; internal/web
// still calls it unchanged via its MonitoringView alias.
func (v MonitoringView) DisksWarnCritCount() int {
	n := 0
	for _, d := range v.Disks {
		if d.UsagePct >= DiskWarnPct {
			n++
		}
	}
	return n
}

// TargetView is one monitorable target as core.API.MonitorTargets reports
// it: a mirror of trinetra.Target's exported fields (ID/Kind/Display/
// Available), kept as its own DTO here (rather than reusing
// trinetra.Target directly) so internal/core -- which imports nothing
// but stdlib + internal/config, see internal/core/doc.go -- never has to
// import internal/trinetra. ID is the namespaced identifier
// (trinetra.Discover's doc: "docker:web", "disk:/", "iface:eth0",
// "temp", "smart:/dev/sda") the SAME config.Config.SetTarget/
// SetTargetThreshold/TargetEnabled/TargetThreshold calls key on, so a
// caller can round-trip a TargetView straight into those setters.
type TargetView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Display   string `json:"display"`
	Available bool   `json:"available"`
}

// AlertRecord is one alert as rendered to a consumer, covering both a
// currently-active alert (trinetra.ActiveAlert, keyed by Key) and a
// historical fire/recover entry (trinetra.AlertEvent): Key identifies
// the check that fired (for example "cpu"), Kind is "fire" or "recover"
// (empty for an active alert, which has no fire/recover distinction of its
// own), Source identifies what raised it, Time is the Unix-seconds
// timestamp it fired (or, for an active alert, went active), and Acked
// mirrors a manual `trinetra alerts ack <key>`.
//
// AckedAt/Title/Delivered are populated only where the underlying record
// actually carries that data (see internal/trinetra/coreapi_alerts.go's
// activeAlertRecords/alertHistoryRecords): an active alert has an ack
// timestamp but no title or delivery outcome of its own (those live on the
// alert-log dispatch record instead), while a history entry has a title and
// delivery outcome but no ack timestamp (the log is a record of past
// fire/recover dispatches, not the current ack state). A field with no
// source for a given record kind is left at its zero value rather than
// invented from another field.
type AlertRecord struct {
	Key      string `json:"key"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Time     int64  `json:"time"`
	Acked    bool   `json:"acked"`
	// AckedAt is the Unix-seconds timestamp a manual `trinetra alerts ack
	// <key>` was recorded (an active alert's ActiveAlert.AckedAt); 0 for a
	// history entry, which carries no ack timestamp.
	AckedAt int64 `json:"acked_at,omitempty"`
	// Title is the human-readable alert title (an alert-log AlertEvent's
	// Title); empty for an active alert, which has no title field of its
	// own (only Source/Reason).
	Title string `json:"title,omitempty"`
	// Delivered is true when a history entry's alert-log dispatch actually
	// reached at least one channel (one of its Delivery records has OK
	// true); false when every attempt failed, none was recorded, or this is
	// an active alert (which carries no delivery outcome of its own).
	Delivered bool `json:"delivered,omitempty"`
	// DeliveredTo names the channels that actually accepted a history
	// entry's dispatch (the Delivery records with OK true), in record order,
	// so a consumer can show WHICH channels a notification reached, not just
	// whether any did. Empty when Delivered is false, and always empty for an
	// active alert (no delivery outcome of its own). This is the per-channel
	// detail the web alerts page previously read from the alert log on disk;
	// carrying it here lets that page go through the control socket instead.
	DeliveredTo []string `json:"delivered_to,omitempty"`
}

// DoctorReport is core's projection of `trinetra doctor`'s diagnostic
// output (internal/trinetra/systemd.go's cmdDoctor): docker reachability,
// smartctl availability, discovered thermal zones and monitoring targets,
// the on/off state of every opt-in extended collector, and a
// human-readable summary of the configured SampleStore's series count and
// on-disk footprint (or "unavailable" if the store failed to open).
type DoctorReport struct {
	// DockerAccess mirrors cmdDoctor's "docker: available=%v method=%s"
	// line as a single string.
	DockerAccess      string `json:"docker_access"`
	SmartctlAvailable bool   `json:"smartctl_available"`
	ThermalZones      int    `json:"thermal_zones"`
	TargetsDiscovered int    `json:"targets_discovered"`

	// ContainerStatsOn/NetThroughputOn/ServicesOn/ProcessesOn/SmartAttrsOn
	// mirror config.Config's identically-purposed *Enabled() methods, one
	// per opt-in extended collector (collectorSummary's on/off line).
	ContainerStatsOn bool `json:"container_stats_on"`
	NetThroughputOn  bool `json:"net_throughput_on"`
	ServicesOn       bool `json:"services_on"`
	ProcessesOn      bool `json:"processes_on"`
	SmartAttrsOn     bool `json:"smart_attrs_on"`

	// StoreStats mirrors collectorSummary's "time-series: N series, X.X MB
	// on disk (raw+1m)" line (or "time-series: unavailable" when the
	// configured store failed to open) as a single string.
	StoreStats string `json:"store_stats"`
	// StoreWarning is a non-empty guardrail message when the series count is
	// abnormally high (#112), e.g. dead Swarm-task series accumulating faster
	// than retention reaps them. Empty when cardinality is healthy.
	StoreWarning string `json:"store_warning,omitempty"`
}

// Event is one live daemon event pushed to a core.API.Subscribe stream:
// Kind identifies what happened (for example "alert_fire", "alert_recover"),
// Severity/Source/Title mirror AlertRecord's identically named fields for
// alert-shaped events, and Time is the Unix-seconds timestamp it occurred.
type Event struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Time     int64  `json:"time"`
}

// HostInfoView is the static host hardware/OS inventory (#100): RAM, CPU model
// and core/thread split, kernel and OS, per-disk hardware, plus the boot time
// and the derived uptime. Served by API.HostInfo; it is static for a boot, so
// it is a dedicated method rather than part of the per-tick DashboardView.
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

// UpdateStatusView is core's projection of a host's self-update posture
// (internal/trinetra/update_cmd.go's updateStatus, itself built from
// internal/update's State/KeySet), for the control-socket UpdateStatus/
// UpdateCheck methods and the web Updates page: Running is this host's
// current build version, Channel/Source mirror config.Config's
// update.channel/update.source, Floor is the version floor that never
// lowers, Available is the newest release the last check found (empty if
// none/not checked yet), Previous is the build kept for rollback (empty if
// there isn't one), Pending is set while an update is staged and awaiting
// its health-guard confirmation, Last is the most recent apply/rollback
// outcome, and KeysLoaded reports whether this build has release keys
// compiled in at all (ProductionKeys() non-empty).
//
// InProgress/LastError (fix round 1, Ruling R10) reflect the control-socket
// implementation's background apply/rollback: UpdateApply/UpdateRollback
// over the socket run their fast checks synchronously then continue in a
// daemon goroutine and return immediately, so a caller must poll
// UpdateStatus to see whether that goroutine is still running (InProgress)
// and, once it finishes, whether it failed (LastError, cleared to "" on a
// clean finish -- checked before Last, which only updates on an apply that
// got far enough to swap a build in). Always false/"" for an implementation
// that runs synchronously (the CLI path, and the file-backed core.API used
// outside a live daemon), since there is nothing async to report.
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
	InProgress bool               `json:"in_progress"`
	LastError  string             `json:"last_error,omitempty"`
}

// UpdatePendingView is core's projection of update.Pending: an update
// currently staged and awaiting its health-guard deadline. Rollback mirrors
// update.Pending.Rollback -- true while a `trinetra update rollback` (or the
// web "Roll back" action) is itself pending confirmation, not a forward
// update.
type UpdatePendingView struct {
	Version  string `json:"version"`
	From     string `json:"from"`
	Deadline int64  `json:"deadline"`
	Rollback bool   `json:"rollback"`
}

// UpdateResultView is core's projection of update.Result: the outcome of the
// most recently confirmed or rolled-back update attempt. Outcome is
// "committed" or "rolled_back"; Detail carries the guard's reason for a
// rollback (empty on a clean commit).
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
