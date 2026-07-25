package serverwatch

import (
	"strings"
)

type Target struct {
	ID        string // namespaced: docker:web, service:nginx.service, disk:/, iface:eth0, temp:zone0, smart:/dev/sda
	Kind      string
	Display   string
	Available bool
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

	// thermal
	if zones, _ := fs.Glob("/sys/class/thermal/thermal_zone*/temp"); len(zones) > 0 {
		for _, z := range zones {
			ts = append(ts, Target{ID: "temp:" + z, Kind: "temp", Display: z, Available: true})
		}
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
