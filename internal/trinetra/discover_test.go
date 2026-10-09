package trinetra

import "testing"

type fakeFS struct {
	files map[string]string
	globs map[string][]string
}

func (f fakeFS) Read(p string) ([]byte, error) {
	v, ok := f.files[p]
	if !ok {
		return nil, errNotExist
	}
	return []byte(v), nil
}
func (f fakeFS) Glob(p string) ([]string, error) { return f.globs[p], nil }

// TestDiscoverLocalReturnsSlice is a smoke test for the exported OS-backed wrapper
// (osExec{}/osFS{} instead of a fake).
func TestDiscoverLocalReturnsSlice(t *testing.T) {
	got := DiscoverLocal()
	if got == nil {
		// nil is a valid "nothing discovered" result on a sandboxed host with
		// no docker/disks/thermal zones/smartctl; just don't want a panic.
		return
	}
}

func TestParseFailedUnits(t *testing.T) {
	s := "nginx.service loaded failed failed A high performance web server\n" +
		"cron.service  loaded failed failed Regular background program\n"
	u := parseFailedUnits(s)
	if len(u) != 2 || u[0] != "nginx.service" {
		t.Fatalf("units = %+v", u)
	}
}

func TestParseUnits(t *testing.T) {
	s := "nginx.service    loaded active   running A high performance web server\n" +
		"cron.service     loaded active   running Regular background program\n" +
		"foo.service      loaded active\n" + // malformed/short line: skip
		"\n" + // blank line: ignored
		"  \n" // whitespace-only line: ignored
	units := parseUnits(s)
	if len(units) != 2 {
		t.Fatalf("len(units) = %d, want 2: %+v", len(units), units)
	}
	got := units[0]
	want := UnitInfo{Name: "nginx.service", Load: "loaded", Active: "active", Sub: "running", Description: "A high performance web server"}
	if got != want {
		t.Fatalf("units[0] = %+v, want %+v", got, want)
	}
	if units[1].Name != "cron.service" || units[1].Description != "Regular background program" {
		t.Fatalf("units[1] = %+v", units[1])
	}
}

func TestDiscoverTempSingleID(t *testing.T) {
	// Two thermal zones present, but Discover must emit exactly ONE target with
	// the plain id "temp" so `monitor disable temp` lines up with buildChecks.
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		return nil, errNotExist // no docker/df/smartctl
	}}
	fs := fakeFS{
		files: map[string]string{},
		globs: map[string][]string{
			"/sys/class/thermal/thermal_zone*/temp": {
				"/sys/class/thermal/thermal_zone0/temp",
				"/sys/class/thermal/thermal_zone1/temp",
			},
		},
	}
	ts := Discover(x, fs)
	var temps []Target
	for _, tg := range ts {
		if tg.Kind == "temp" {
			temps = append(temps, tg)
		}
	}
	if len(temps) != 1 {
		t.Fatalf("want exactly 1 temp target, got %+v", temps)
	}
	if temps[0].ID != "temp" {
		t.Fatalf("temp id = %q, want %q", temps[0].ID, "temp")
	}
	if !temps[0].Available {
		t.Fatal("temp target should be Available")
	}
}

// TestDiscoverFiltersPseudoAndOverlayMounts asserts Discover applies the same isRealMount
// && isRealFsType gate as collectSlow (via the typed `df -PT`).
func TestDiscoverFiltersPseudoAndOverlayMounts(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		if name == "df" && len(args) > 0 && args[0] == "-PT" {
			return []byte("Filesystem     Type     1-blocks   Used   Available Capacity Mounted on\n" +
				"/dev/sda1      ext4     100        60     40        60% /\n" +
				"/dev/sda2      ext4     200        20     180       10% /boot\n" +
				"overlay        overlay  999        999    0         100% /var/lib/docker/overlay2/abc/merged\n" +
				"/dev/loop0     squashfs 12         12     0         100% /snap/core/1234\n" +
				"tmpfs          tmpfs    1000       0      1000      0% /dev/shm\n"), nil
		}
		return nil, errNotExist // no docker/smartctl/other df variants
	}}
	fs := fakeFS{}
	ts := Discover(x, fs)
	var disks []string
	for _, tg := range ts {
		if tg.Kind == "disk" {
			disks = append(disks, tg.Display)
		}
	}
	want := map[string]bool{"/": true, "/boot": true}
	if len(disks) != len(want) {
		t.Fatalf("disk targets = %v, want exactly %v", disks, want)
	}
	for _, d := range disks {
		if !want[d] {
			t.Errorf("Discover surfaced junk disk target %q", d)
		}
	}
}

func TestParseSmartScan(t *testing.T) {
	s := "/dev/sda -d sat # /dev/sda [SAT], ATA device\n/dev/sdb -d sat # ...\n"
	d := parseSmartScan(s)
	if len(d) != 2 || d[0] != "/dev/sda" {
		t.Fatalf("devices = %+v", d)
	}
}

// TestIsRealFsType asserts the fstype denylist: real block-device
// filesystems (ext4/xfs/btrfs/...) pass, pseudo/virtual/container
// filesystems (overlay, tmpfs, squashfs, proc, ...) are rejected, matching
// case-insensitively, with fuseblk kept but other fuse.* rejected.
func TestIsRealFsType(t *testing.T) {
	real := []string{"ext2", "ext3", "ext4", "xfs", "btrfs", "zfs", "vfat", "exfat", "f2fs", "ntfs", "reiserfs", "jfs", "EXT4", "fuseblk"}
	for _, f := range real {
		if !isRealFsType(f) {
			t.Errorf("isRealFsType(%q) = false, want true", f)
		}
	}
	pseudo := []string{
		"overlay", "overlay2", "tmpfs", "devtmpfs", "squashfs", "nsfs",
		"proc", "sysfs", "cgroup", "cgroup2", "mqueue", "hugetlbfs",
		"tracefs", "securityfs", "pstore", "fusectl", "debugfs", "configfs",
		"bpf", "autofs", "binfmt_misc", "ramfs", "rpc_pipefs",
		"fuse.sshfs", "FUSE.glusterfs", "OVERLAY", "",
	}
	for _, f := range pseudo {
		if isRealFsType(f) {
			t.Errorf("isRealFsType(%q) = true, want false", f)
		}
	}
}

// TestIsRealMountRejectsContainerAndSnapPrefixes is defense-in-depth alongside
// isRealFsType: even if a mount's reported fstype looked real-ish.
func TestIsRealMountRejectsContainerAndSnapPrefixes(t *testing.T) {
	rejected := []string{
		"/var/lib/docker/overlay2/abc123/merged",
		"/var/lib/docker/",
		"/var/lib/containers/storage/overlay/def456/merged",
		"/var/lib/kubelet/pods/xyz/volumes",
		"/snap/core/1234",
		"/snap/",
		"/var/snap/lxd/common",
	}
	for _, m := range rejected {
		if isRealMount(m) {
			t.Errorf("isRealMount(%q) = true, want false", m)
		}
	}
	kept := []string{"/", "/boot", "/mnt/data", "/home"}
	for _, m := range kept {
		if !isRealMount(m) {
			t.Errorf("isRealMount(%q) = false, want true", m)
		}
	}
}
