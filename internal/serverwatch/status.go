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
