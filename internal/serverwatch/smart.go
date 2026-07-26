package serverwatch

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

// parseSmartAttrs parses `smartctl -A <dev>` output: the classic ATA SMART
// attribute table, one row per attribute in the fixed column layout "ID#
// ATTRIBUTE_NAME FLAG VALUE WORST THRESH TYPE UPDATED WHEN_FAILED RAW_VALUE"
// (10 whitespace-separated fields; any trailing annotation like "(Min/Max
// 20/45)" on the RAW_VALUE column lands in extra fields past index 9 and is
// ignored). It is deliberately tolerant: vendor/HDD-vs-SSD attribute sets
// vary, so any attribute this function doesn't find in the input just
// leaves that SmartAttr field at its zero value rather than erroring.
//
//   - Temperature_Celsius / Airflow_Temperature -> TempC, from RAW_VALUE.
//   - Wear_Leveling_Count / Media_Wearout_Indicator -> WearPct, derived as
//     100-VALUE (the normalized VALUE column is smartmontools' 100=fresh,
//     declining-toward-0 life-remaining score, so this converts it to a
//     "percent worn" figure matching the field name). Percentage_Used
//     (occasionally surfaced as a synthetic ATA-style row on NVMe-oriented
//     tooling) is already a "percent worn" figure, so it's taken directly
//     from RAW_VALUE with no inversion.
//   - Reallocated_Sector_Ct -> ReallocSectors, from RAW_VALUE.
//
// The first matching row wins for TempC/WearPct (Temperature_Celsius is
// preferred over Airflow_Temperature simply by appearing first in typical
// smartctl output; same for the wear attributes).
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
