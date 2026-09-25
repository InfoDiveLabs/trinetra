package trinetra

import (
	"strconv"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestPublicIPGatedByConfig pins #102: the local IP is always collected, and the
// outbound public-IP lookup runs only when collect.public_ip is enabled.
func TestPublicIPGatedByConfig(t *testing.T) {
	origL, origP := lookupLocalIP, lookupPublicIP
	lookupLocalIP = func() string { return "192.168.1.50" }
	lookupPublicIP = func() string { return "203.0.113.7" }
	defer func() { lookupLocalIP, lookupPublicIP = origL, origP }()

	off := config.Default() // public_ip defaults off
	h := collectHostInfoFor(off)
	if h.LocalIP != "192.168.1.50" {
		t.Errorf("LocalIP = %q, want it always collected", h.LocalIP)
	}
	if h.PublicIP != "" {
		t.Errorf("PublicIP = %q, want empty when collect.public_ip is off", h.PublicIP)
	}

	on := config.Default()
	tru := true
	on.Collect.PublicIP = &tru
	if got := collectHostInfoFor(on).PublicIP; got != "203.0.113.7" {
		t.Errorf("PublicIP = %q, want the looked-up IP when enabled", got)
	}
}

func TestParseCPUInfoIntelHyperthreaded(t *testing.T) {
	// Two logical processors sharing one physical socket with 1 core each is
	// unrealistic; use a realistic 1 socket / 2 cores / 4 threads layout.
	s := `processor	: 0
model name	: Intel(R) Core(TM) i5-8250U CPU @ 1.60GHz
physical id	: 0
cpu cores	: 2
cpu MHz		: 1600.000
processor	: 1
model name	: Intel(R) Core(TM) i5-8250U CPU @ 1.60GHz
physical id	: 0
cpu cores	: 2
processor	: 2
physical id	: 0
cpu cores	: 2
processor	: 3
physical id	: 0
cpu cores	: 2
`
	model, sockets, cores, threads, mhz := parseCPUInfo(s)
	if model != "Intel(R) Core(TM) i5-8250U CPU @ 1.60GHz" {
		t.Errorf("model = %q", model)
	}
	if sockets != 1 {
		t.Errorf("sockets = %d, want 1", sockets)
	}
	if cores != 2 {
		t.Errorf("cores = %d, want 2", cores)
	}
	if threads != 4 {
		t.Errorf("threads = %d, want 4", threads)
	}
	if mhz != 1600.0 {
		t.Errorf("baseMHz = %v, want 1600", mhz)
	}
}

func TestParseCPUInfoDualSocket(t *testing.T) {
	// Two physical Xeon sockets, 2 cores / 4 threads each (trimmed): the box has
	// 2 sockets, 4 cores total, 8 threads total. Mirrors the infodivelabs host
	// where a dual-socket machine was reporting only its aggregate core count.
	var b strings.Builder
	for proc := 0; proc < 8; proc++ {
		phys := 0
		if proc >= 4 {
			phys = 1
		}
		b.WriteString("processor\t: ")
		b.WriteString(strconv.Itoa(proc))
		b.WriteString("\nmodel name\t: Intel(R) Xeon(R) Platinum 8153 CPU @ 2.00GHz\nphysical id\t: ")
		b.WriteString(strconv.Itoa(phys))
		b.WriteString("\ncpu cores\t: 2\n\n")
	}
	_, sockets, cores, threads, _ := parseCPUInfo(b.String())
	if sockets != 2 {
		t.Errorf("sockets = %d, want 2 (dual-socket box)", sockets)
	}
	if cores != 4 {
		t.Errorf("cores = %d, want 4 (2 per socket x 2 sockets)", cores)
	}
	if threads != 8 {
		t.Errorf("threads = %d, want 8", threads)
	}
}

func TestKeepDisk(t *testing.T) {
	cases := []struct {
		device, fstype string
		keep           bool
	}{
		{"/dev/sda1", "ext4", true},           // local block device
		{"/dev/nvme0n1p2", "ext4", true},      // nvme partition
		{"/dev/mapper/vg-root", "ext4", true}, // LVM volume
		{"nas:/vol/media", "nfs4", true},      // network mount kept
		{"//smb/share", "cifs", true},         // cifs kept
		{"overlay", "overlay", false},         // docker layer dropped
		{"tmpfs", "tmpfs", false},             // ram-backed dropped
		{"/dev/loop3", "squashfs", false},     // snap dropped
		{"udev", "devtmpfs", false},           // pseudo dropped
	}
	for _, c := range cases {
		if got := keepDisk(c.device, c.fstype); got != c.keep {
			t.Errorf("keepDisk(%q,%q) = %v, want %v", c.device, c.fstype, got, c.keep)
		}
	}
}

func TestParseCPUInfoARMFallback(t *testing.T) {
	// Raspberry Pi style: no "model name"/"cpu cores"; cores fall back to the
	// logical count, model to the "Model" line.
	s := `processor	: 0
processor	: 1
processor	: 2
processor	: 3
Hardware	: BCM2835
Model		: Raspberry Pi 4 Model B Rev 1.4
`
	model, sockets, cores, threads, _ := parseCPUInfo(s)
	if model != "Raspberry Pi 4 Model B Rev 1.4" {
		t.Errorf("model = %q, want the Pi Model line", model)
	}
	if threads != 4 || cores != 4 {
		t.Errorf("cores=%d threads=%d, want 4/4 (fallback)", cores, threads)
	}
	if sockets != 1 {
		t.Errorf("sockets = %d, want 1 (no physical id -> single package)", sockets)
	}
}

func TestParseOSRelease(t *testing.T) {
	s := "NAME=\"Debian GNU/Linux\"\nPRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\n"
	if got := parseOSRelease(s); got != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("parseOSRelease = %q", got)
	}
	if got := parseOSRelease("ID=whatever\n"); got != "" {
		t.Errorf("no PRETTY_NAME should yield empty, got %q", got)
	}
}

func TestBaseBlockDevice(t *testing.T) {
	cases := []struct {
		dev  string
		want string
		ok   bool
	}{
		{"/dev/sda1", "sda", true},
		{"/dev/sda", "sda", true},
		{"/dev/nvme0n1p2", "nvme0n1", true},
		{"/dev/nvme0n1", "nvme0n1", true},
		{"/dev/mmcblk0p1", "mmcblk0", true},
		{"/dev/dm-0", "", false},
		{"/dev/mapper/vg-root", "", false},
		{"overlay", "", false},
		{"tmpfs", "", false},
	}
	for _, c := range cases {
		got, ok := baseBlockDevice(c.dev)
		if got != c.want || ok != c.ok {
			t.Errorf("baseBlockDevice(%q) = (%q,%v), want (%q,%v)", c.dev, got, ok, c.want, c.ok)
		}
	}
}

func TestCollectHostInfo(t *testing.T) {
	fs := fakeFS{files: map[string]string{
		"/proc/sys/kernel/osrelease":      "6.1.0-13-amd64\n",
		"/etc/os-release":                 "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\n",
		"/proc/cpuinfo":                   "processor\t: 0\nmodel name\t: Test CPU\nphysical id\t: 0\ncpu cores\t: 4\ncpu MHz\t: 2400.0\nprocessor\t: 1\nphysical id\t: 0\ncpu cores\t: 4\n",
		"/proc/meminfo":                   "MemTotal:        8192000 kB\nMemAvailable:    4096000 kB\n",
		"/proc/stat":                      "cpu 1 2 3\nbtime 1700000000\n",
		"/sys/block/sda/device/model":     "Samsung SSD 860\n",
		"/sys/block/sda/queue/rotational": "0\n",
	}}
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		// df -PT -B1 with one real disk, one tmpfs, and one docker overlay: only
		// the real disk should survive the keepDisk filter.
		return []byte("Filesystem     Type  1B-blocks       Used   Available Use% Mounted on\n" +
			"/dev/sda1      ext4  512000000000  1000000     511000000000   1% /\n" +
			"tmpfs          tmpfs   8192000000        0       8192000000   0% /run\n" +
			"overlay        overlay 512000000000 1000000    511000000000   1% /var/lib/docker/overlay2/abc/merged\n"), nil
	}}

	h := collectHostInfo(x, fs)
	if h.Kernel != "6.1.0-13-amd64" {
		t.Errorf("kernel = %q", h.Kernel)
	}
	if h.OS != "Debian GNU/Linux 12 (bookworm)" {
		t.Errorf("os = %q", h.OS)
	}
	if h.CPUModel != "Test CPU" || h.CPUCores != 4 || h.CPUThreads != 2 {
		t.Errorf("cpu = %q cores=%d threads=%d", h.CPUModel, h.CPUCores, h.CPUThreads)
	}
	if h.MemTotalBytes != 8192000*1024 {
		t.Errorf("mem = %d, want %d", h.MemTotalBytes, 8192000*1024)
	}
	if h.BootTime != 1700000000 {
		t.Errorf("bootTime = %d, want 1700000000", h.BootTime)
	}
	// Find the sda disk and assert model/rotational; tmpfs contributes a row too.
	var sda *HostDisk
	for i := range h.Disks {
		if h.Disks[i].Device == "sda" {
			sda = &h.Disks[i]
		}
	}
	if sda == nil {
		t.Fatalf("sda disk not collected: %+v", h.Disks)
	}
	if sda.Model != "Samsung SSD 860" || sda.Rotational {
		t.Errorf("sda = %q rotational=%v, want SSD non-rotational", sda.Model, sda.Rotational)
	}
	if sda.FSType != "ext4" || sda.Mount != "/" {
		t.Errorf("sda fstype=%q mount=%q", sda.FSType, sda.Mount)
	}
	// The tmpfs and docker overlay rows must have been filtered out.
	if len(h.Disks) != 1 {
		t.Errorf("want only the real disk kept, got %d rows: %+v", len(h.Disks), h.Disks)
	}
}

func TestBuildHostInfoViewUptime(t *testing.T) {
	h := HostInfo{BootTime: 1000, CPUCores: 2, Disks: []HostDisk{{Device: "sda"}}}
	v := buildHostInfoView(h, 1000+3600)
	if v.UptimeSec != 3600 {
		t.Errorf("UptimeSec = %d, want 3600", v.UptimeSec)
	}
	if len(v.Disks) != 1 || v.Disks[0].Device != "sda" {
		t.Errorf("disks not mapped: %+v", v.Disks)
	}
	// Unknown boot time -> uptime 0, no underflow.
	if got := buildHostInfoView(HostInfo{}, 5000).UptimeSec; got != 0 {
		t.Errorf("uptime with zero boot time = %d, want 0", got)
	}
}
