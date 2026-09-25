package trinetra

import (
	"strconv"
	"strings"
)

type dockerAccess struct {
	method    string // "socket" | "group" | "sudo"
	sudo      bool
	available bool
	swarm     bool // this node is an active Swarm member (#118)
}

const dockerPSFormat = "{{.Names}}\t{{.State}}\t{{.Status}}"

func probeDocker(x Exec, fs FileSource) dockerAccess {
	// Try a plain `docker ps` first (works as root or with group membership).
	if _, err := x.Run("docker", "ps", "-a", "--format", dockerPSFormat); err == nil {
		method := "socket"
		if _, e := fs.Read("/var/run/docker.sock"); e != nil {
			method = "group"
		}
		da := dockerAccess{method: method, available: true}
		da.swarm = probeSwarm(x, false)
		return da
	}
	// Fall back to sudo.
	if _, err := x.Run("sudo", "docker", "ps", "-a", "--format", dockerPSFormat); err == nil {
		da := dockerAccess{method: "sudo", sudo: true, available: true}
		da.swarm = probeSwarm(x, true)
		return da
	}
	return dockerAccess{available: false}
}

// probeSwarm reports whether this node is an ACTIVE Swarm member (#118), via
// `docker info --format {{.Swarm.LocalNodeState}}` == "active". Gates the
// task->service collapse so a plain-docker host is unaffected. Any error (old
// docker, format unsupported) is treated as not-swarm.
func probeSwarm(x Exec, sudo bool) bool {
	args := []string{"info", "--format", "{{.Swarm.LocalNodeState}}"}
	var out []byte
	var err error
	if sudo {
		out, err = x.Run("sudo", append([]string{"docker"}, args...)...)
	} else {
		out, err = x.Run("docker", args...)
	}
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

func (a dockerAccess) list(x Exec) ([]Container, error) {
	var out []byte
	var err error
	if a.sudo {
		out, err = x.Run("sudo", "docker", "ps", "-a", "--format", dockerPSFormat)
	} else {
		out, err = x.Run("docker", "ps", "-a", "--format", dockerPSFormat)
	}
	if err != nil {
		return nil, err
	}
	return parseDockerPS(string(out)), nil
}

type Container struct {
	Name     string
	State    string
	Restarts int
}

func parseDockerPS(s string) []Container {
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		cs = append(cs, Container{Name: f[0], State: f[1]})
	}
	return cs
}

// dockerStatsFormat mirrors dockerPSFormat above: a --format string for
// `docker stats --no-stream` that produces one tab-separated line per
// container, parsed by parseDockerStats.
const dockerStatsFormat = "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}"

// ContainerStat is one container's point-in-time resource usage, as parsed
// from `docker stats --no-stream`.
type ContainerStat struct {
	Name    string
	CPUPct  float64
	MemMiB  float64
	NetRxMB float64
	NetTxMB float64
}

// stats runs `docker stats --no-stream` via the access method probeDocker
// recorded (plain or sudo), mirroring list's dispatch above.
func (a dockerAccess) stats(x Exec) ([]ContainerStat, error) {
	var out []byte
	var err error
	if a.sudo {
		out, err = x.Run("sudo", "docker", "stats", "--no-stream", "--format", dockerStatsFormat)
	} else {
		out, err = x.Run("docker", "stats", "--no-stream", "--format", dockerStatsFormat)
	}
	if err != nil {
		return nil, err
	}
	return parseDockerStats(string(out)), nil
}

// validContainerName reports whether name is a plausible docker container name
// or id: docker's own charset ([A-Za-z0-9][A-Za-z0-9_.-]*). Everything the
// logs path shells out is validated against this AND against the live
// container list (see inprocAPI.ContainerLogs), so a caller cannot smuggle a
// flag ("--since", "-f") or another argument through the name. Belt and
// suspenders on top of exec.Command's no-shell argv, which already prevents
// shell metacharacter injection.
func validContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '-':
			if i == 0 { // docker names never start with a separator
				return false
			}
		default:
			return false
		}
	}
	return true
}

// logs returns the last `lines` log lines of container name via
// `docker logs --tail N`, dispatching plain/sudo like list and stats. The
// caller is responsible for having validated name (validContainerName) and
// confirmed it is a live container; logs merges stdout+stderr because docker
// writes container output to both.
func (a dockerAccess) logs(x Exec, name string, lines int) (string, error) {
	if lines <= 0 {
		lines = 200
	}
	tail := strconv.Itoa(lines)
	var out []byte
	var err error
	if a.sudo {
		out, err = x.Run("sudo", "docker", "logs", "--tail", tail, name)
	} else {
		out, err = x.Run("docker", "logs", "--tail", tail, name)
	}
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// parseDockerStats parses dockerStatsFormat output ("name\tcpu%\tmemUsage\t
// netIO" per line) into ContainerStat values. Tolerant of odd/missing
// fields: any line that doesn't have all 4 tab-separated fields, or whose
// CPU%/mem/net sub-fields don't parse, is skipped rather than aborting the
// whole batch (a single misbehaving container must not blank out every
// other container's stats).
func parseDockerStats(s string) []ContainerStat {
	var out []ContainerStat
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		cpuPct, ok := parsePercent(f[1])
		if !ok {
			continue
		}
		memMiB, ok := parseMemUsage(f[2])
		if !ok {
			continue
		}
		rxMB, txMB, ok := parseNetIO(f[3])
		if !ok {
			continue
		}
		out = append(out, ContainerStat{
			Name:    f[0],
			CPUPct:  cpuPct,
			MemMiB:  memMiB,
			NetRxMB: rxMB,
			NetTxMB: txMB,
		})
	}
	return out
}

// parsePercent parses docker's CPUPerc field, e.g. "11.20%" -> 11.2.
func parsePercent(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseMemUsage parses docker's MemUsage field, e.g. "512MiB / 16GiB", and
// returns the "used" side (before the "/") in MiB.
func parseMemUsage(s string) (float64, bool) {
	used, _, ok := splitSlash(s)
	if !ok {
		return 0, false
	}
	return parseBinarySize(used)
}

// parseNetIO parses docker's NetIO field, e.g. "2.1MB / 0.4MB", returning
// (rx, tx) in MB (decimal, 1000-based, matching docker's own formatting).
func parseNetIO(s string) (rxMB, txMB float64, ok bool) {
	rx, tx, ok := splitSlash(s)
	if !ok {
		return 0, 0, false
	}
	rxMB, ok1 := parseDecimalSize(rx)
	txMB, ok2 := parseDecimalSize(tx)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return rxMB, txMB, true
}

// splitSlash splits a docker "used / total" style field on " / ", trimming
// whitespace from both sides.
func splitSlash(s string) (left, right string, ok bool) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

// binarySizeUnits maps docker's binary (1024-based) size suffixes to their
// value in MiB. Ordered longest-suffix-first: every shorter suffix here is
// also a suffix of the longer ones (e.g. "B" ends "GiB"/"MiB"/"KiB" too), so
// callers checking in this order find the correct (longest) match first.
var binarySizeUnits = []struct {
	suffix string
	toMiB  float64
}{
	{"GiB", 1024},
	{"MiB", 1},
	{"KiB", 1.0 / 1024},
	{"B", 1.0 / (1024 * 1024)},
}

// parseBinarySize parses a docker binary-unit size like "512MiB", "1.5GiB",
// "900KiB", or "512B" into MiB.
func parseBinarySize(s string) (float64, bool) {
	for _, u := range binarySizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0, false
			}
			return n * u.toMiB, true
		}
	}
	return 0, false
}

// decimalSizeUnits maps docker's decimal (1000-based) NetIO size suffixes to
// their value in MB. Ordered longest-suffix-first for the same reason as
// binarySizeUnits above ("B" is a suffix of "kB"/"MB"/"GB" too).
var decimalSizeUnits = []struct {
	suffix string
	toMB   float64
}{
	{"GB", 1000},
	{"MB", 1},
	{"kB", 1.0 / 1000},
	{"B", 1.0 / (1000 * 1000)},
}

// parseDecimalSize parses a docker decimal-unit size like "2.1MB", "500kB",
// "1.2GB", or "900B" into MB.
func parseDecimalSize(s string) (float64, bool) {
	for _, u := range decimalSizeUnits {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0, false
			}
			return n * u.toMB, true
		}
	}
	return 0, false
}
