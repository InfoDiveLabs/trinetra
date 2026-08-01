package serverwatch

import (
	"sort"
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
// into the SampleStore as a series -- full unit-name cardinality per host
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

// DiscoverLocal enumerates monitorable targets on THIS host using the real
// OS-backed Exec/FileSource (os/exec, os.ReadFile, filepath.Glob) -- the
// same probes cmdMonitor (systemd.go) runs for `serverwatch monitor list`.
// Exported so a caller guaranteed to run on the same host as the daemon it
// is managing -- serverwatch-ctl, whose control socket is always a local
// unix socket (internal/control), never a network one -- can list targets
// for its monitor-thresholds screen without duplicating Discover's exec/fs
// plumbing or routing target discovery through core.API (which would mean
// running these same df/docker/smartctl probes on every core.API.Monitoring()
// call, including the web dashboard's Monitoring page poll -- see the
// beta-2 B2 task 2 report for why that path was rejected). See Discover for
// the general, dependency-injected form cmdMonitor and this package's own
// tests use.
func DiscoverLocal() []Target {
	return Discover(osExec{}, osFS{})
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

	// filesystems: use the TYPED df (`df -PT`) so we can apply the same
	// isRealMount && isRealFsType gate collectSlow uses to fill snap.Disks.
	// Without the fstype gate, a root daemon on a docker host would surface
	// one `disk:<overlay>` target per container (plus squashfs/tmpfs/nsfs
	// pseudo-mounts) in `monitor list`/`monitor threshold`, none of which
	// ever populate snap.Disks -- the two paths must agree on what a real
	// disk is (see collectSlow and fix-disk-telegram-brief.md).
	if out, err := x.Run("df", "-PT"); err == nil {
		typed := parseDFTypes(string(out))
		mounts := make([]string, 0, len(typed))
		for mount := range typed {
			mounts = append(mounts, mount)
		}
		sort.Strings(mounts) // stable target order (df map iteration is random)
		for _, mount := range mounts {
			if isRealMount(mount) && isRealFsType(typed[mount].FsType) {
				ts = append(ts, Target{ID: "disk:" + mount, Kind: "disk", Display: mount, Available: true})
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
	// Defense-in-depth: reject known container/snap runtime mount roots even
	// if isRealFsType's fstype denylist somehow doesn't catch them (e.g. a
	// bind-mount or future overlay driver reporting a real-looking fstype).
	// A root daemon on a docker host otherwise sees one mount per container
	// under /var/lib/docker/overlay2/<hash>/merged -- this is the field bug
	// that motivated this whole filter (see fix-disk-telegram-brief.md).
	for _, p := range []string{
		"/var/lib/docker/", "/var/lib/containers/", "/var/lib/kubelet/",
		"/snap/", "/var/snap/",
	} {
		if strings.HasPrefix(m, p) {
			return false
		}
	}
	return true
}

// isRealFsType reports whether fstype names a real, user-facing block-device
// filesystem (ext2/3/4, xfs, btrfs, zfs, vfat, exfat, f2fs, ntfs, reiserfs,
// jfs, ...) as opposed to a pseudo/virtual/container filesystem (overlay,
// tmpfs, proc, sysfs, cgroup, squashfs, ...). This is the robust
// discriminator for the docker-host field bug: `df` on a root daemon lists
// one `overlay` mount per container plus assorted pseudo-filesystems, and
// path-prefix filtering (isRealMount) alone can't catch mounts outside the
// known container-runtime directories, so this denylists filesystem TYPES
// instead. Case-insensitive since some platforms/tools report fstype in
// mixed case. "fuse.*" is treated as pseudo (FUSE-backed virtual/network
// mounts like fuse.sshfs) except "fuseblk", which backs real block-device
// filesystems (e.g. NTFS-3G) and is kept.
func isRealFsType(fstype string) bool {
	if fstype == "" {
		return false
	}
	f := strings.ToLower(fstype)
	if f == "fuseblk" {
		return true
	}
	if f == "fuse" || strings.HasPrefix(f, "fuse.") {
		return false
	}
	switch f {
	case "overlay", "overlay2", "aufs",
		"tmpfs", "devtmpfs", "ramfs",
		"squashfs", "nsfs",
		"proc", "sysfs", "cgroup", "cgroup2", "mqueue", "hugetlbfs",
		"tracefs", "securityfs", "pstore", "fusectl", "debugfs", "configfs",
		"bpf", "autofs", "binfmt_misc", "rpc_pipefs":
		return false
	}
	return true
}

// runMaybeSudo tries a command directly, then via sudo.
func runMaybeSudo(x Exec, name string, args ...string) ([]byte, error) {
	if out, err := x.Run(name, args...); err == nil {
		return out, nil
	}
	return x.Run("sudo", append([]string{name}, args...)...)
}
