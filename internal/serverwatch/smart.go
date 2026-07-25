package serverwatch

import "strings"

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
