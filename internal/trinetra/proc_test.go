package trinetra

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// procStatLine builds a realistic /proc/<pid>/stat line for tests, matching the field
// layout parseProcPidStat expects.
func procStatLine(pid int, comm, state string, utime, stime uint64, threads int, rss int64) string {
	rest := []string{
		state,                         // 3 state
		"1", "0", "0", "0", "-1", "0", // 4-9 ppid..flags
		"10", "20", "0", "0", // 10-13 minflt..cmajflt
		strconv.FormatUint(utime, 10), // 14 utime
		strconv.FormatUint(stime, 10), // 15 stime
		"0", "0", "20", "0",           // 16-19 cutime..nice
		strconv.Itoa(threads),   // 20 num_threads
		"0", "999999", "123456", // 21-23 itrealvalue..vsize
		strconv.FormatInt(rss, 10), // 24 rss
	}
	return fmt.Sprintf("%d (%s) %s", pid, comm, strings.Join(rest, " "))
}

func TestParseProcPidStat(t *testing.T) {
	// comm deliberately contains both a space and nested parens, mirroring
	// e.g. a kernel thread comm like "(sd-pam)" embedded in a longer name.
	line := procStatLine(1234, "(sd-pam) x", "S", 100, 50, 4, 640)
	comm, state, utime, stime, threads, rss, ok := parseProcPidStat(line)
	if !ok {
		t.Fatalf("ok = false, want true for line %q", line)
	}
	if comm != "(sd-pam) x" {
		t.Errorf("comm = %q, want %q", comm, "(sd-pam) x")
	}
	if state != "S" {
		t.Errorf("state = %q, want %q", state, "S")
	}
	if utime != 100 || stime != 50 {
		t.Errorf("utime/stime = %d/%d, want 100/50", utime, stime)
	}
	if threads != 4 {
		t.Errorf("threads = %d, want 4", threads)
	}
	if rss != 640 {
		t.Errorf("rss = %d, want 640", rss)
	}
}

func TestParseProcPidStatMalformed(t *testing.T) {
	cases := []string{
		"",
		"1234 (bash) S 1 0",  // way too short after comm
		"1234 bash S 1 0 0",  // no parens at all
		"1234 (bash S 1 0 0", // unbalanced parens (no closing paren)
	}
	for _, c := range cases {
		if _, _, _, _, _, _, ok := parseProcPidStat(c); ok {
			t.Errorf("parseProcPidStat(%q) ok = true, want false", c)
		}
	}
}

func TestProcCPUCalcFirstCallZero(t *testing.T) {
	var c ProcCPUCalc
	pct := c.Pct(map[int]uint64{1: 100, 2: 200}, 1000)
	if got := pct[1]; got != 0 {
		t.Errorf("pct[1] = %v, want 0 on first call", got)
	}
	if got := pct[2]; got != 0 {
		t.Errorf("pct[2] = %v, want 0 on first call", got)
	}
}

func TestProcCPUCalcComputesDelta(t *testing.T) {
	var c ProcCPUCalc
	c.Pct(map[int]uint64{1: 100}, 1000)
	pct := c.Pct(map[int]uint64{1: 150}, 1100) // proc +50 jiffies, total +100
	if got := pct[1]; got != 50 {
		t.Errorf("pct[1] = %v, want 50", got)
	}
}

func TestProcCPUCalcTotalDeltaNonPositiveIsZero(t *testing.T) {
	var c ProcCPUCalc
	c.Pct(map[int]uint64{1: 100}, 1000)
	pct := c.Pct(map[int]uint64{1: 150}, 1000) // total unchanged: totalDelta<=0
	if got := pct[1]; got != 0 {
		t.Errorf("pct[1] = %v, want 0 when totalDelta<=0", got)
	}
}

func TestProcCPUCalcNewPidIsZero(t *testing.T) {
	var c ProcCPUCalc
	c.Pct(map[int]uint64{1: 100}, 1000)
	pct := c.Pct(map[int]uint64{1: 150, 2: 999}, 1100) // pid 2 is new this tick
	if got := pct[2]; got != 0 {
		t.Errorf("pct[2] = %v, want 0 for a pid absent from the previous sample", got)
	}
}

func TestCollectProcesses(t *testing.T) {
	fs := fakeFS{
		globs: map[string][]string{
			"/proc/[0-9]*/stat": {
				"/proc/1/stat",
				"/proc/2/stat",
				"/proc/3/stat",
				"/proc/4/stat", // no matching file below: Read errors, must be skipped
			},
		},
		files: map[string]string{
			"/proc/1/stat": procStatLine(1, "init", "R", 50, 50, 1, 100), // running, 100 pages rss
			"/proc/2/stat": procStatLine(2, "sshd", "S", 10, 10, 2, 50),  // sleeping, 50 pages rss
			"/proc/3/stat": procStatLine(3, "zombie", "Z", 0, 0, 1, 0),   // zombie, 0 rss
			"/proc/stat":   "cpu 1000 0 0 9000 0 0 0 0 0 0\n",
		},
	}
	var calc ProcCPUCalc
	snap := collectProcesses(fs, &calc, 4) // 4KB pages

	if snap.Total != 3 {
		t.Errorf("Total = %d, want 3 (pid 4's Read error must be skipped, not panic)", snap.Total)
	}
	if snap.Running != 1 {
		t.Errorf("Running = %d, want 1", snap.Running)
	}
	if snap.Sleeping != 1 {
		t.Errorf("Sleeping = %d, want 1", snap.Sleeping)
	}
	if snap.Zombie != 1 {
		t.Errorf("Zombie = %d, want 1", snap.Zombie)
	}
	if len(snap.Top) != 3 {
		t.Fatalf("len(Top) = %d, want 3", len(snap.Top))
	}
	// First call to calc: no prior sample, so every pid's CPU% is 0 -> Top falls back to
	// sorting by mem descending: pid1 (100*4/1024 MiB) first, then pid2, then pid3 (0 MiB).
	if snap.Top[0].PID != 1 || snap.Top[1].PID != 2 || snap.Top[2].PID != 3 {
		t.Fatalf("Top order = %+v, want pids [1,2,3] by mem desc on first tick", snap.Top)
	}
	wantMiB := float64(100*4) / 1024
	if got := snap.Top[0].MemMiB; got != wantMiB {
		t.Errorf("Top[0].MemMiB = %v, want %v", got, wantMiB)
	}
	if snap.Top[0].Threads != 1 {
		t.Errorf("Top[0].Threads = %d, want 1", snap.Top[0].Threads)
	}
	if snap.Top[0].Name != "init" {
		t.Errorf("Top[0].Name = %q, want %q", snap.Top[0].Name, "init")
	}
}

func TestCollectProcessesTopBounded(t *testing.T) {
	globs := make([]string, 0, 20)
	files := map[string]string{}
	for i := 1; i <= 20; i++ {
		p := fmt.Sprintf("/proc/%d/stat", i)
		globs = append(globs, p)
		files[p] = procStatLine(i, "proc", "S", 0, 0, 1, int64(i))
	}
	fs := fakeFS{
		globs: map[string][]string{"/proc/[0-9]*/stat": globs},
		files: files,
	}
	var calc ProcCPUCalc
	snap := collectProcesses(fs, &calc, 4)
	if snap.Total != 20 {
		t.Fatalf("Total = %d, want 20", snap.Total)
	}
	if len(snap.Top) != procTopN {
		t.Fatalf("len(Top) = %d, want %d (bounded)", len(snap.Top), procTopN)
	}
}
