package serverwatch

import (
	"fmt"
	"sort"
	"strings"
)

// DiskDetail is the live per-mount filesystem detail beyond the plain
// usage-percentage carried by Snapshot.Disks: device path, filesystem type,
// inode usage, free/total bytes, and (when the SampleStore has enough
// history) a linear fill-rate projection. Populated by collectSlow from
// `df -PT -B1` (device/fstype/usage/size/free) merged with `df -Pi`
// (inode%), keyed by mount — see parseDFTypes/parseDFInodes in collect.go
// and projectDaysToFull in projection.go. Additive/live only: the existing
// Snapshot.Disks map is untouched since alerting (buildSlowChecks) and the
// SampleStore series (slowMetricSet) both depend on it.
type DiskDetail struct {
	Device, FsType       string
	UsagePct, InodePct   float64
	FreeBytes, SizeBytes uint64
	// DaysToFull/DaysToFullKnown are a linear-projection estimate of how many
	// days remain until the mount reaches 100% used, computed by
	// projectDaysToFull from the mount's "disk:<mount>" SampleStore series.
	// DaysToFullKnown is false when the projection isn't meaningful (a nil
	// store, fewer than 2 history points, or a flat/declining trend).
	DaysToFull      float64
	DaysToFullKnown bool
}

// SmartAttr is a device's parsed `smartctl -A` attributes: temperature,
// wear/life-remaining percentage, and reallocated-sector count. See
// parseSmartAttrs in smart.go. Fields are tolerant of absence (0 = not
// reported by this device) since attribute sets vary by vendor/SSD vs HDD.
type SmartAttr struct {
	TempC          int
	WearPct        int
	ReallocSectors int
}

type Snapshot struct {
	TS           int64              `json:"ts"`
	CPU          float64            `json:"cpu"`
	MemPct       float64            `json:"mem_pct"`
	SwapPct      float64            `json:"swap_pct"`
	Load1        float64            `json:"load1"`
	Load5        float64            `json:"load5"`
	Load15       float64            `json:"load15"`
	TempC        float64            `json:"temp_c"`
	Disks        map[string]float64 `json:"disks"`
	Online       bool               `json:"online"`
	DockerAccess string             `json:"docker_access"`
	Containers   map[string]string  `json:"containers,omitempty"`   // name -> state (e.g. "running","exited")
	FailedUnits  []string           `json:"failed_units,omitempty"` // systemctl --failed unit names
	SmartHealth  map[string]string  `json:"smart_health,omitempty"` // device -> "PASSED"|"FAILED"|"UNKNOWN"
	// DiskDetail is the live per-mount device/fstype/inode%/size detail (see
	// the DiskDetail type doc comment above), keyed by mount. Additive to
	// Disks, always collected in collectSlow (no config toggle — matches how
	// Disks itself has no toggle).
	DiskDetail map[string]DiskDetail `json:"disk_detail,omitempty"`
	// SmartAttrs is the live per-device SMART attribute detail (temperature,
	// wear%, reallocated sectors; see the SmartAttr type doc comment above),
	// keyed by device path. Populated alongside SmartHealth for every
	// discovered SMART device.
	SmartAttrs map[string]SmartAttr `json:"smart_attrs,omitempty"`
	// ContainerStats is the live per-container cpu%/mem/net snapshot from
	// `docker stats --no-stream` (opt-in via collect.container_stats,
	// slow-tier only). Keyed by container name. See docker.go/daemon.go
	// (dockerAccess.stats, containerMetricSet) for the collector and the
	// bounded (cpu+mem only) series this feeds into the SampleStore.
	ContainerStats map[string]ContainerStat `json:"container_stats,omitempty"`
	// NetRates is the live per-interface network throughput (bytes/sec),
	// computed by NetRateCalc from consecutive /proc/net/dev samples
	// (opt-in via collect.net_throughput, slow-tier only, see daemon.go's
	// sampler loop and net.go). Empty/nil on the very first slow tick (no
	// prior sample to diff against yet) and whenever the collector is
	// disabled. Keyed by interface name.
	NetRates map[string]IfaceRate `json:"net_rates,omitempty"`
	// Units is the live, full systemd service-unit inventory (opt-in via
	// collect.services, slow-tier only, see daemon.go collectSlow and
	// discover.go listUnits/parseUnits), for the Monitoring "services" tab.
	// Deliberately a snapshot only — NOT fed into the SampleStore as a
	// series (unit-name cardinality) — unlike FailedUnits above, which
	// continues to drive service:* alerting unchanged.
	Units []UnitInfo `json:"units,omitempty"`
	// Processes is the live process-table overview (counts + top-N by
	// CPU/mem), computed by collectProcesses (opt-in via collect.processes,
	// slow-tier only, see daemon.go's sampler loop and proc.go). Deliberately
	// a snapshot only for the Monitoring "processes" tab — NOT fed into the
	// SampleStore as a series, since per-process cardinality (hundreds of
	// short-lived pids per host) is exactly the trap this design avoids,
	// mirroring Units above.
	Processes ProcSnapshot `json:"processes,omitempty"`
}

func renderStatus(s Snapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CPU %.0f%%  mem %.0f%%  swap %.0f%%  load %.2f", s.CPU, s.MemPct, s.SwapPct, s.Load1)
	if s.TempC > 0 {
		fmt.Fprintf(&b, "  temp %.0f°C", s.TempC)
	}
	fmt.Fprintf(&b, "\ninternet: %s", onlineStr(s.Online))
	if len(s.Disks) > 0 {
		mts := make([]string, 0, len(s.Disks))
		for m := range s.Disks {
			mts = append(mts, m)
		}
		sort.Strings(mts)
		b.WriteString("\ndisks:")
		for _, m := range mts {
			fmt.Fprintf(&b, "\n  %s %.0f%%", m, s.Disks[m])
		}
	}
	return b.String()
}

func renderDisks(disks map[string]float64) string {
	if len(disks) == 0 {
		return "no filesystems discovered"
	}
	mts := make([]string, 0, len(disks))
	for m := range disks {
		mts = append(mts, m)
	}
	sort.Strings(mts)
	var b strings.Builder
	b.WriteString("disks:")
	for _, m := range mts {
		fmt.Fprintf(&b, "\n  %s %.0f%%", m, disks[m])
	}
	return b.String()
}

func renderDocker(cs map[string]string) string {
	if len(cs) == 0 {
		return "docker: no containers (or unavailable)"
	}
	names := make([]string, 0, len(cs))
	for n := range cs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("containers:")
	for _, n := range names {
		mark := "✅"
		if cs[n] != "running" {
			mark = "❌"
		}
		fmt.Fprintf(&b, "\n  %s %s (%s)", mark, n, cs[n])
	}
	return b.String()
}

func renderServices(units []string) string {
	if len(units) == 0 {
		return "systemd: no failed units ✅"
	}
	var b strings.Builder
	b.WriteString("failed units:")
	for _, u := range units {
		fmt.Fprintf(&b, "\n  ❌ %s", u)
	}
	return b.String()
}

func onlineStr(up bool) string {
	if up {
		return "up ✅"
	}
	return "DOWN ❌"
}
