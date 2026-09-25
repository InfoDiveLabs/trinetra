// Package trinetra: hostinfo.go collects static host hardware/OS inventory
// (#100): RAM, CPU model and core/thread split, kernel and OS, per-disk model
// and rotational type, and the host boot time (uptime is derived at read time).
// All reads go through the injected Exec/FileSource seam so the parsers are
// unit-testable from fixture strings with no host access, matching collect.go's
// parseMeminfo/parseDFTypes style. Everything here is stdlib-only.
package trinetra

import (
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// HostInfo is the static (per-boot) host inventory. It is collected once at
// daemon startup and cached; uptime is NOT stored (it is derived from BootTime
// at read time so a cached value stays correct across reads).
type HostInfo struct {
	Hostname      string
	Kernel        string // /proc/sys/kernel/osrelease
	OS            string // /etc/os-release PRETTY_NAME
	CPUModel      string
	CPUSockets    int     // distinct physical CPU packages (2 on a dual-socket box)
	CPUCores      int     // physical cores (summed across sockets)
	CPUThreads    int     // logical processors
	CPUBaseMHz    float64 // approximate, from /proc/cpuinfo "cpu MHz"
	MemTotalBytes uint64
	BootTime      int64 // unix seconds; uptime = now - BootTime
	Disks         []HostDisk
	// LocalIP is the host's primary non-loopback IPv4 (#102), always collected.
	LocalIP string
	// PublicIP is the host's internet-facing IP (#102), populated only when
	// collect.public_ip is enabled and the outbound lookup succeeded.
	PublicIP string
}

// lookupLocalIP/lookupPublicIP are seams so tests inject IPs without touching
// the network. Production uses the defaults below.
var (
	lookupLocalIP  = defaultLocalIP
	lookupPublicIP = defaultPublicIP
)

// defaultLocalIP returns the host's primary local IPv4: the first global-unicast
// IPv4 on an up, non-loopback interface, or "" if none.
func defaultLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && ip4.IsGlobalUnicast() && !ip4.IsLinkLocalUnicast() {
				return ip4.String()
			}
		}
	}
	return ""
}

// publicIPEndpoint is the third-party echo service the opt-in public-IP lookup
// dials. It returns the caller's IP as plain text.
const publicIPEndpoint = "https://api.ipify.org"

// defaultPublicIP fetches the host's internet-facing IP from publicIPEndpoint,
// bounded by a short timeout and a tiny response cap. Any failure (offline,
// timeout, non-2xx, unparseable) yields "".
func defaultPublicIP() string {
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get(publicIPEndpoint)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
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
func parseCPUInfo(s string) (model string, sockets, cores, threads int, baseMHz float64) {
	var armModel string
	// physical id -> cpu cores, so a multi-socket box counts each socket's
	// cores once rather than summing per logical thread. physIDs is the set of
	// distinct sockets seen, so a dual-socket box reports Sockets=2 even when
	// the two sockets are identical.
	coresByPhys := map[string]int{}
	physIDs := map[string]bool{}
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
			physIDs[val] = true
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
	sockets = len(physIDs)
	if sockets == 0 && threads > 0 {
		sockets = 1 // no "physical id" line (ARM, containers): a single package
	}
	return model, sockets, cores, threads, baseMHz
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
		h.CPUModel, h.CPUSockets, h.CPUCores, h.CPUThreads, h.CPUBaseMHz = parseCPUInfo(string(b))
	}
	if b, err := fs.Read("/proc/meminfo"); err == nil {
		if m, err := parseMeminfo(string(b)); err == nil {
			h.MemTotalBytes = m.TotalKB * 1024
		}
	}
	if bt, ok := hostBootTime(fs); ok {
		h.BootTime = bt.Unix()
	}
	h.LocalIP = lookupLocalIP()
	h.Disks = collectHostDisks(x, fs)
	return h
}

// pseudoFSTypes are the kernel/virtual/container filesystems that back no real
// storage and would otherwise clutter the disk list: docker's overlay layers,
// RAM-backed mounts, snap's squashfs loops, and the /proc-family pseudo mounts.
// They are dropped from the host disk inventory.
var pseudoFSTypes = map[string]bool{
	"overlay": true, "tmpfs": true, "devtmpfs": true, "squashfs": true,
	"ramfs": true, "aufs": true, "proc": true, "sysfs": true, "cgroup": true,
	"cgroup2": true, "mqueue": true, "debugfs": true, "tracefs": true,
	"fusectl": true, "configfs": true, "pstore": true, "bpf": true, "nsfs": true,
	"binfmt_misc": true, "autofs": true, "efivarfs": true, "hugetlbfs": true,
	"devpts": true, "securityfs": true, "fuse.lxcfs": true, "none": true,
}

// networkFSTypes are remote filesystems that ARE real storage even though they
// are not local block devices; they are kept (per the "also keep network
// mounts" choice).
var networkFSTypes = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smbfs": true, "smb3": true,
	"ceph": true, "glusterfs": true, "9p": true, "fuse.sshfs": true,
	"fuse.rclone": true, "beegfs": true, "lustre": true,
}

// keepDisk decides whether a df row is a real disk worth showing: a network
// filesystem (kept regardless of device), or a genuine local block device
// (a /dev/ path that is not a snap/loop mount). Everything else -- overlay,
// tmpfs, squashfs, and the other pseudo filesystems -- is dropped.
func keepDisk(device, fstype string) bool {
	if networkFSTypes[fstype] {
		return true
	}
	if pseudoFSTypes[fstype] {
		return false
	}
	base, ok := baseBlockDevice(device)
	if !ok {
		// device-mapper / LVM report base "" but are real storage; keep any
		// remaining /dev/-backed mount that is not a loop device.
		return strings.HasPrefix(device, "/dev/") && !strings.HasPrefix(device, "/dev/loop")
	}
	return !strings.HasPrefix(base, "loop")
}

// collectHostDisks runs df -PT -B1 once and enriches each mount with its base
// block device's model and rotational flag from /sys/block. Only real disks are
// kept (see keepDisk): local block devices and network mounts, not docker
// overlay / tmpfs / snap squashfs. Deduplicated per base device so several
// partitions of one disk report once.
func collectHostDisks(x Exec, fs FileSource) []HostDisk {
	out, err := x.Run("df", "-PT", "-B1")
	if err != nil {
		return nil
	}
	details := parseDFTypes(string(out))
	seen := map[string]bool{}
	var disks []HostDisk
	for mount, d := range details {
		if !keepDisk(d.Device, d.FsType) {
			continue
		}
		base, ok := baseBlockDevice(d.Device)
		key := base
		if !ok {
			key = "mount:" + mount // network / mapper mount: keep per-mount
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
