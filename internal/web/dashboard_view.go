//go:build web

package web

// DashboardView is internal/web's own projection of the daemon's live
// Snapshot: exactly the fields templates/dashboard.html (and the /events SSE
// stream, sse.go) need to render the dashboard, expressed with no
// serverwatch import.
//
// Deps.Snapshot returns this type rather than serverwatch.Snapshot itself
// because internal/web must never import internal/serverwatch (see the
// design note atop internal/serverwatch/web_deps.go — that would be a
// direct import cycle once internal/serverwatch/daemon_web.go imports
// internal/web to wire the two together). daemon_web.go — which imports
// BOTH packages precisely because it is the seam — builds one of these by
// copying values out of a serverwatch.Snapshot (buildDashboardView); this
// package only ever consumes the finished value.
//
// Every field here is a copy (scalars, or a freshly built slice) taken from
// the Snapshot at adapt time, never a map/slice alias into it: the adapter
// only reads the Snapshot it's given, exactly as the concurrency contract in
// internal/serverwatch/web_deps.go's snapshotHub doc requires (readers must
// never mutate a published Snapshot's map fields — see that file for why
// that invariant is what makes the atomic.Pointer safe without a lock).
type DashboardView struct {
	// TS is the Unix-seconds timestamp the daemon's sampler loop stamped
	// onto this snapshot (Snapshot.TS) — how stale the view is.
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
	// this in — it isn't part of Snapshot itself), for the CPU tile's "N
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
	// enabled — UnitsTotal is 0 otherwise even if UnitsFailed isn't).
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
	// building this view doesn't have a downtime EventsStore to compute it
	// from (e.g. a bare `DashboardView{}` in a test that doesn't care about
	// the strip).
	Availability Availability `json:"availability"`
}

// ProcessCounts mirrors serverwatch.ProcSnapshot's aggregate counts (Top is
// deliberately not carried here — the dashboard only shows counts, not a
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
// color — a display-only threshold for the dashboard's summary tiles,
// independent of (and not a substitute for) the daemon's own configurable
// alert thresholds (internal/config), which keep driving real alert
// delivery. Exported so internal/serverwatch/daemon_web.go's adapter
// (buildDashboardView) can count DisksCritical using the exact same cutoff
// this package's template uses to color the same mounts.
const DiskCriticalPct = 90.0

// DiskWarnPct is the warn-level counterpart to DiskCriticalPct, same
// display-only caveat.
const DiskWarnPct = 70.0
