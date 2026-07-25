package serverwatch

import (
	"strings"
)

type Target struct {
	ID        string // namespaced: docker:web, service:nginx.service, disk:/, iface:eth0, temp, smart:/dev/sda
	Kind      string
	Display   string
	Available bool
}

// UnitInfo is one systemd service unit's live state, as parsed from
// `systemctl list-units --type=service --all --plain --no-legend`. This is a
// full inventory (not just failures) for the Monitoring "services" tab, kept
// deliberately snapshot-only: see listUnits/collectSlow for why it is never
// persisted to the SampleStore as a series.
type UnitInfo struct {
	Name        string
	Load        string
	Active      string
	Sub         string
	Description string
}

// parseUnits parses `systemctl list-units --type=service --all --plain
// --no-legend` output. Each line is UNIT LOAD ACTIVE SUB DESCRIPTION,
// whitespace-separated with DESCRIPTION free-form (may itself contain
// spaces), so only the first 4 fields are split out positionally and the
// remainder of the line is joined back as Description. Blank/whitespace-only
// lines and any line with fewer than 5 fields (malformed/truncated output)
// are skipped rather than aborting the whole batch.
func parseUnits(s string) []UnitInfo {
	var out []UnitInfo
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		out = append(out, UnitInfo{
			Name:        f[0],
			Load:        f[1],
			Active:      f[2],
			Sub:         f[3],
			Description: strings.Join(f[4:], " "),
		})
	}
	return out
}

// listUnits runs `systemctl list-units` (via runMaybeSudo: systemctl is
// usually root-accessible without sudo, but the sudo fallback is harmless if
// it isn't) and parses the full unit inventory. Snapshot-only: unlike
// parseFailedUnits below (used for --failed alerting), this is never fed
// into the SampleStore as a series — full unit-name cardinality per host
// makes that a bad fit for time-series storage.
func listUnits(x Exec) ([]UnitInfo, error) {
	out, err := runMaybeSudo(x, "systemctl", "list-units", "--type=service", "--all", "--plain", "--no-legend")
	if err != nil {
		return nil, err
	}
	return parseUnits(string(out)), nil
}

func parseFailedUnits(s string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if strings.HasSuffix(f[0], ".service") || strings.HasSuffix(f[0], ".timer") || strings.HasSuffix(f[0], ".mount") {
			out = append(out, f[0])
		}
	}
	return out
}

// Discover enumerates all monitorable targets on the host.
func Discover(x Exec, fs FileSource) []Target {
	var ts []Target

	// docker
	da := probeDocker(x, fs)
	if da.available {
		if cs, err := da.list(x); err == nil {
			for _, c := range cs {
				ts = append(ts, Target{ID: "docker:" + c.Name, Kind: "docker", Display: c.Name, Available: true})
			}
		}
	} else {
		ts = append(ts, Target{ID: "docker", Kind: "docker", Display: "docker (unavailable)", Available: false})
	}

	// filesystems
	if out, err := x.Run("df", "-PB1"); err == nil {
		for _, d := range mustDF(string(out)) {
			if isRealMount(d.Mount) {
				ts = append(ts, Target{ID: "disk:" + d.Mount, Kind: "disk", Display: d.Mount, Available: true})
			}
		}
	}

	// interfaces
	if b, err := fs.Read("/proc/net/dev"); err == nil {
		for _, ifc := range parseNetDevNames(string(b)) {
			ts = append(ts, Target{ID: "iface:" + ifc, Kind: "iface", Display: ifc, Available: true})
		}
	}

	// thermal: collectSnapshot only reads zone[0] into a single snap.TempC, and
	// buildChecks keys the anomaly check on the plain id "temp". Emit exactly ONE
	// target with that same id so `monitor disable temp` / `monitor threshold
	// temp 70` line up with the check (per-zone ids were a silent no-op).
	if zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp"); len(zones) > 0 {
		display := "cpu-thermal"
		if zones[0] != "" {
			display = zones[0]
		}
		ts = append(ts, Target{ID: "temp", Kind: "temp", Display: display, Available: true})
	}

	// smart
	if out, err := runMaybeSudo(x, "smartctl", "--scan"); err == nil {
		for _, dev := range parseSmartScan(string(out)) {
			ts = append(ts, Target{ID: "smart:" + dev, Kind: "smart", Display: dev, Available: true})
		}
	}

	return ts
}

func isRealMount(m string) bool {
	for _, p := range []string{"/proc", "/sys", "/dev", "/run"} {
		if m == p || strings.HasPrefix(m, p+"/") {
			return false
		}
	}
	return true
}

func mustDF(s string) []DiskUsage { d, _ := parseDF(s); return d }

// runMaybeSudo tries a command directly, then via sudo.
func runMaybeSudo(x Exec, name string, args ...string) ([]byte, error) {
	if out, err := x.Run(name, args...); err == nil {
		return out, nil
	}
	return x.Run("sudo", append([]string{name}, args...)...)
}
