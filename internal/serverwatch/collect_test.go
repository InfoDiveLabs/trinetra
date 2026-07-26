package serverwatch

import (
	"math"
	"testing"
)

func TestParseProcStatAndBusy(t *testing.T) {
	prev, err := parseProcStat("cpu  100 0 200 700 0 0 0 0 0 0\n")
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := parseProcStat("cpu  200 0 400 1400 0 0 0 0 0 0\n")
	// delta total = 1000, delta idle = 700 -> busy 30%
	if got := cpuBusyPct(prev, cur); math.Abs(got-30) > 0.001 {
		t.Fatalf("busy = %v want 30", got)
	}
}

func TestParseMeminfo(t *testing.T) {
	s := "MemTotal:       1000 kB\nMemAvailable:    250 kB\nSwapTotal:       200 kB\nSwapFree:        150 kB\n"
	m, err := parseMeminfo(s)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(m.UsedPct()-75) > 0.001 {
		t.Fatalf("used = %v want 75", m.UsedPct())
	}
	if math.Abs(m.SwapUsedPct()-25) > 0.001 {
		t.Fatalf("swap used = %v want 25", m.SwapUsedPct())
	}
}

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15, err := parseLoadavg("0.50 1.00 2.00 1/234 5678\n")
	if err != nil || l1 != 0.5 || l5 != 1.0 || l15 != 2.0 {
		t.Fatalf("load = %v %v %v err=%v", l1, l5, l15, err)
	}
}

func TestParseDF(t *testing.T) {
	// df -PB1 style: Filesystem 1-blocks Used Available Capacity Mounted-on
	s := "Filesystem 1-blocks Used Available Capacity Mounted on\n" +
		"/dev/sda1 100 90 10 90% /\n" +
		"tmpfs 50 0 50 0% /run\n"
	d, err := parseDF(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 {
		t.Fatalf("rows = %d", len(d))
	}
	if d[0].Mount != "/" || math.Abs(d[0].UsedPct-90) > 0.001 || d[0].FreeBytes != 10 {
		t.Fatalf("row0 = %+v", d[0])
	}
}

func TestParseDFTypes(t *testing.T) {
	s := "Filesystem     Type  1-blocks      Used Available Capacity Mounted on\n" +
		"/dev/sda1      ext4       100        90        10       90% /\n" +
		"tmpfs          tmpfs       50         0        50        0% /run\n" +
		"malformed line with too few fields\n"
	d := parseDFTypes(s)
	if len(d) != 2 {
		t.Fatalf("rows = %d, want 2 (malformed line skipped): %+v", len(d), d)
	}
	root, ok := d["/"]
	if !ok {
		t.Fatalf("missing mount / in %+v", d)
	}
	if root.Device != "/dev/sda1" || root.FsType != "ext4" {
		t.Fatalf("root = %+v, want device /dev/sda1 fstype ext4", root)
	}
	if math.Abs(root.UsagePct-90) > 0.001 || root.FreeBytes != 10 || root.SizeBytes != 100 {
		t.Fatalf("root = %+v", root)
	}
	run, ok := d["/run"]
	if !ok || run.FsType != "tmpfs" || run.Device != "tmpfs" {
		t.Fatalf("run = %+v, want fstype/device tmpfs", run)
	}
}

func TestParseDFInodes(t *testing.T) {
	s := "Filesystem      Inodes   IUsed   IFree IUse% Mounted on\n" +
		"/dev/sda1        1000     900     100   90% /\n" +
		"tmpfs             500       0     500    0% /run\n" +
		"special             -       -       -    - /proc\n" +
		"short line\n"
	m := parseDFInodes(s)
	if len(m) != 2 {
		t.Fatalf("rows = %d, want 2 (non-numeric + short lines skipped): %+v", len(m), m)
	}
	if math.Abs(m["/"]-90) > 0.001 {
		t.Fatalf("m[/] = %v, want 90", m["/"])
	}
	if math.Abs(m["/run"]-0) > 0.001 {
		t.Fatalf("m[/run] = %v, want 0", m["/run"])
	}
	if _, ok := m["/proc"]; ok {
		t.Fatalf("m[/proc] should be absent (non-numeric IUse%%): %+v", m)
	}
}

func TestParseThermal(t *testing.T) {
	c, err := parseThermal("52000\n")
	if err != nil || math.Abs(c-52) > 0.001 {
		t.Fatalf("temp = %v err=%v", c, err)
	}
}
