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

func onlineStr(up bool) string {
	if up {
		return "up ✅"
	}
	return "DOWN ❌"
}
