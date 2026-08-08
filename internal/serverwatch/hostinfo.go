// Package serverwatch: hostinfo.go collects static host hardware/OS inventory
// (#100): RAM, CPU model and core/thread split, kernel and OS, per-disk model
// and rotational type, and the host boot time (uptime is derived at read time).
// All reads go through the injected Exec/FileSource seam so the parsers are
// unit-testable from fixture strings with no host access, matching collect.go's
// parseMeminfo/parseDFTypes style. Everything here is stdlib-only.
package serverwatch

import (
	"os"
	"strconv"
	"strings"
)

// HostInfo is the static (per-boot) host inventory. It is collected once at
// daemon startup and cached; uptime is NOT stored (it is derived from BootTime
// at read time so a cached value stays correct across reads).
type HostInfo struct {
	Hostname      string
	Kernel        string // /proc/sys/kernel/osrelease
	OS            string // /etc/os-release PRETTY_NAME
	CPUModel      string
	CPUCores      int     // physical cores
	CPUThreads    int     // logical processors
	CPUBaseMHz    float64 // approximate, from /proc/cpuinfo "cpu MHz"
	MemTotalBytes uint64
	BootTime      int64 // unix seconds; uptime = now - BootTime
	Disks         []HostDisk
}

// HostDisk is one physical/block disk backing a mounted filesystem.
type HostDisk struct {
	Device     string // base block device, e.g. "sda", "nvme0n1"
	Model      string
	Rotational bool // true = spinning HDD, false = SSD/NVMe/unknown
	SizeBytes  uint64
	FSType     string
	Mount      string
}

// parseCPUInfo extracts the CPU model, physical core count, logical thread
// count, and an approximate base MHz from /proc/cpuinfo. It tolerates the ARM
// layout (Raspberry Pi and friends), which has no "model name"/"cpu cores"
// lines: model falls back to the "Model" line and cores to the logical count.
func parseCPUInfo(s string) (model string, cores, threads int, baseMHz float64) {
	var armModel string
	// physical id -> cpu cores, so a multi-socket box counts each socket's
	// cores once rather than summing per logical thread.
	coresByPhys := map[string]int{}
	curPhys := ""
	for _, line := range strings.Split(s, "\n") {
		key, val, ok := splitCPUInfoLine(line)
		if !ok {
			continue
		}
		switch key {
		case "processor":
			threads++
		case "model name":
			if model == "" {
				model = val
			}
		case "Model": // ARM /proc/cpuinfo
			armModel = val
		case "physical id":
			curPhys = val
		case "cpu cores":
			if n, err := strconv.Atoi(val); err == nil {
				coresByPhys[curPhys] = n
			}
		case "cpu MHz":
			if baseMHz == 0 {
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					baseMHz = f
				}
			}
		}
	}
	if model == "" {
		model = armModel
	}
	for _, n := range coresByPhys {
		cores += n
	}
	if cores == 0 {
		cores = threads // no "cpu cores" line (ARM, containers): assume 1 thread/core
	}
	return model, cores, threads, baseMHz
}

// splitCPUInfoLine splits a "key : value" /proc/cpuinfo line, trimming both
// sides. ok is false for blank lines or lines without a colon.
func splitCPUInfoLine(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:i])
	val = strings.TrimSpace(line[i+1:])
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

// parseOSRelease returns the PRETTY_NAME value from /etc/os-release content
// (quotes stripped), or "" if absent.
func parseOSRelease(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "PRETTY_NAME="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

// baseBlockDevice maps a df device path (e.g. "/dev/sda1", "/dev/nvme0n1p2") to
// its base block device name for a /sys/block lookup ("sda", "nvme0n1"). ok is
// false for non-/dev devices (overlay, tmpfs, mapper), which have no
// /sys/block entry and simply report no model/rotational.
func baseBlockDevice(dev string) (string, bool) {
	name, ok := strings.CutPrefix(dev, "/dev/")
	if !ok || name == "" {
		return "", false
	}
	// device-mapper / LVM (dm-0, mapper/...) have no simple base block device.
	if strings.HasPrefix(name, "dm-") || strings.HasPrefix(name, "mapper/") {
		return "", false
	}
	// nvme/mmcblk/loop base devices END in a digit (nvme0n1, mmcblk0) and use a
	// "p<N>" partition suffix, so trailing digits are part of the base name and
	// must NOT be stripped -- only a "p<digits>" suffix is a partition.
	if strings.HasPrefix(name, "nvme") || strings.HasPrefix(name, "mmcblk") || strings.HasPrefix(name, "loop") {
		if i := strings.LastIndexByte(name, 'p'); i > 0 && isDigit(name[i-1]) && allDigits(name[i+1:]) {
			return name[:i], true // nvme0n1p2 -> nvme0n1, mmcblk0p1 -> mmcblk0
		}
		return name, true // already a base device (nvme0n1, mmcblk0)
	}
	// sd/vd/hd/xvd style: base is letters, a partition is a trailing digit run.
	i := len(name)
	for i > 0 && isDigit(name[i-1]) {
		i--
	}
	if i > 0 {
		return name[:i], true // sda1 -> sda, sda -> sda
	}
	return name, true
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

// collectHostInfo assembles the static host inventory from the injected
// Exec/FileSource. Every read is best-effort: a missing file or command leaves
// the corresponding field zero rather than failing the whole collection, so
// this works on a minimal container, an ARM box, or a non-Linux dev host
// (where it simply returns mostly-empty).
func collectHostInfo(x Exec, fs FileSource) HostInfo {
	var h HostInfo
	h.Hostname, _ = os.Hostname()

	if b, err := fs.Read("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if b, err := fs.Read("/etc/os-release"); err == nil {
		h.OS = parseOSRelease(string(b))
	}
	if b, err := fs.Read("/proc/cpuinfo"); err == nil {
		h.CPUModel, h.CPUCores, h.CPUThreads, h.CPUBaseMHz = parseCPUInfo(string(b))
	}
	if b, err := fs.Read("/proc/meminfo"); err == nil {
		if m, err := parseMeminfo(string(b)); err == nil {
			h.MemTotalBytes = m.TotalKB * 1024
		}
	}
	if bt, ok := hostBootTime(fs); ok {
		h.BootTime = bt.Unix()
	}
	h.Disks = collectHostDisks(x, fs)
	return h
}

// collectHostDisks runs df -PT -B1 once and enriches each mount with its base
// block device's model and rotational flag from /sys/block. Non-/dev mounts
// (overlay, tmpfs, mapper) contribute a disk row with size/fstype but no
// model/rotational. Deduplicated per base device so several partitions of one
// disk report once.
func collectHostDisks(x Exec, fs FileSource) []HostDisk {
	out, err := x.Run("df", "-PT", "-B1")
	if err != nil {
		return nil
	}
	details := parseDFTypes(string(out))
	seen := map[string]bool{}
	var disks []HostDisk
	for mount, d := range details {
		base, ok := baseBlockDevice(d.Device)
		key := base
		if !ok {
			key = "mount:" + mount // no base device; keep per-mount so overlay/tmpfs still show
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		hd := HostDisk{Device: base, SizeBytes: d.SizeBytes, FSType: d.FsType, Mount: mount}
		if ok {
			if b, err := fs.Read("/sys/block/" + base + "/device/model"); err == nil {
				hd.Model = strings.TrimSpace(string(b))
			}
			if b, err := fs.Read("/sys/block/" + base + "/queue/rotational"); err == nil {
				hd.Rotational = strings.TrimSpace(string(b)) == "1"
			}
		}
		disks = append(disks, hd)
	}
	return disks
}
