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
	// serverwatch.PickResolution (the age/config-dependent picker, which
	// stays in internal/serverwatch since it needs the store's actual
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
// need to render the dashboard, expressed with no serverwatch import.
//
// Deps.Snapshot returns this type (aliased in internal/web as
// web.DashboardView) rather than serverwatch.Snapshot itself because this
// package must never import internal/serverwatch (see doc.go's import
// contract). internal/serverwatch/daemon_web.go -- which imports BOTH
// packages precisely because it is the seam -- builds one of these by
// copying values out of a serverwatch.Snapshot (buildDashboardView); web
// only ever consumes the finished value via the alias.
//
// Every field here is a copy (scalars, or a freshly built slice) taken from
// the Snapshot at adapt time, never a map/slice alias into it: the adapter
// only reads the Snapshot it's given, exactly as the concurrency contract in
// internal/serverwatch/web_deps.go's snapshotHub doc requires (readers must
// never mutate a published Snapshot's map fields -- see that file for why
// that invariant is what makes the atomic.Pointer safe without a lock).
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
}

// ProcessCounts mirrors serverwatch.ProcSnapshot's aggregate counts (Top is
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
	// DaysToFull/DaysToFullKnown mirror serverwatch.DiskDetail's linear-fill
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
// delivery. Exported so internal/serverwatch/daemon_web.go's adapter
// (buildDashboardView) can count DisksCritical using the exact same cutoff
// this package's template uses to color the same mounts.
const DiskCriticalPct = 90.0

// DiskWarnPct is the warn-level counterpart to DiskCriticalPct, same
// display-only caveat.
const DiskWarnPct = 70.0

// MonitoringView is core's projection of the daemon's live Snapshot for the
// /monitoring detail page (ported from ui-mockup/monitoring.html): the
// Containers/Units/Processes/Filesystems tables, expressed with no
// serverwatch import -- the same seam DashboardView already established for
// the dashboard. See that type's doc for why the projection crosses the
// core <-> serverwatch boundary this way instead of serverwatch.Snapshot
// itself, and for the map-safety/copy-only contract
// internal/serverwatch/daemon_web.go's buildMonitoringView adapter must
// honor when building one of these.
type MonitoringView struct {
	// Containers is every container the daemon's plain state listing knows
	// about (serverwatch.Snapshot.Containers, name -> state), each merged
	// with its live docker-stats row (ContainerStats) when present -- see
	// MonitoringContainerView.HasStats for when cpu/mem/net are meaningful.
	Containers []MonitoringContainerView `json:"containers,omitempty"`

	// FailedUnits is the systemd units currently in a failed state
	// (serverwatch.Snapshot.FailedUnits) -- always populated regardless of
	// UnitsEnabled below, since `systemctl --failed` is always collected
	// (the same alerting input service:* checks use), independent of the
	// opt-in full-inventory collector.
	FailedUnits []string `json:"failed_units,omitempty"`
	// Units is the full systemd unit inventory (serverwatch.Snapshot.Units),
	// populated only when UnitsEnabled -- the page renders a "collector
	// disabled" note in its place otherwise, rather than an empty table
	// that looks like "zero units" (which is never really true).
	Units []MonitoringUnitView `json:"units,omitempty"`
	// UnitsEnabled mirrors config.Config.ServicesEnabled() (collect.services)
	// at adapt time.
	UnitsEnabled bool `json:"units_enabled"`

	// Processes is the top-N process list (serverwatch.Snapshot.Processes.Top),
	// populated only when ProcessesEnabled -- same "collector disabled" note
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
	// (serverwatch.DiskDetail's linear-fill projection).
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

// AlertRecord is one alert as rendered to a consumer, covering both a
// currently-active alert (serverwatch.ActiveAlert, keyed by Key) and a
// historical fire/recover entry (serverwatch.AlertEvent): Key identifies
// the check that fired (for example "cpu"), Kind is "fire" or "recover"
// (empty for an active alert, which has no fire/recover distinction of its
// own), Source identifies what raised it, Time is the Unix-seconds
// timestamp it fired (or, for an active alert, went active), and Acked
// mirrors a manual `serverwatch alerts ack <key>`.
//
// AckedAt/Title/Delivered are populated only where the underlying record
// actually carries that data (see internal/serverwatch/coreapi_alerts.go's
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
	// AckedAt is the Unix-seconds timestamp a manual `serverwatch alerts ack
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
}

// DoctorReport is core's projection of `serverwatch doctor`'s diagnostic
// output (internal/serverwatch/systemd.go's cmdDoctor): docker reachability,
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
