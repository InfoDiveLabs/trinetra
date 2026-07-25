package serverwatch

import (
	"fmt"
	"sort"
	"strings"
)

type Snapshot struct {
	TS           int64              `json:"ts"`
	CPU          float64            `json:"cpu"`
	MemPct       float64            `json:"mem_pct"`
	SwapPct      float64            `json:"swap_pct"`
	Load1        float64            `json:"load1"`
	TempC        float64            `json:"temp_c"`
	Disks        map[string]float64 `json:"disks"`
	Online       bool               `json:"online"`
	DockerAccess string             `json:"docker_access"`
	Containers   map[string]string  `json:"containers,omitempty"`   // name -> state (e.g. "running","exited")
	FailedUnits  []string           `json:"failed_units,omitempty"` // systemctl --failed unit names
	SmartHealth  map[string]string  `json:"smart_health,omitempty"` // device -> "PASSED"|"FAILED"|"UNKNOWN"
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
