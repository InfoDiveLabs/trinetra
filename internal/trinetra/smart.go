package trinetra

import (
	"strconv"
	"strings"
)

// parseSmartScan extracts device paths from `smartctl --scan` output.
func parseSmartScan(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Fields(line)[0])
	}
	return out
}

// parseSmartHealth reads `smartctl -H <dev>`; returns (passed, recognized).
func parseSmartHealth(s string) (bool, bool) {
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "passed"):
		return true, true
	case strings.Contains(low, "failed"):
		return false, true
	}
	return false, false
}

// parseSmartAttrs parses `smartctl -A <dev>` output: the classic ATA SMART attribute table.
func parseSmartAttrs(s string) SmartAttr {
	var a SmartAttr
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		value, errV := strconv.Atoi(f[3])
		raw, errR := strconv.Atoi(f[9])
		switch f[1] {
		case "Temperature_Celsius", "Airflow_Temperature":
			if a.TempC == 0 && errR == nil {
				a.TempC = raw
			}
		case "Wear_Leveling_Count", "Media_Wearout_Indicator":
			if a.WearPct == 0 && errV == nil {
				a.WearPct = 100 - value
			}
		case "Percentage_Used":
			if a.WearPct == 0 && errR == nil {
				a.WearPct = raw
			}
		case "Reallocated_Sector_Ct":
			if errR == nil {
				a.ReallocSectors = raw
			}
		}
	}
	return a
}
