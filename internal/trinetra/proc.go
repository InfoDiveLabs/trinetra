package trinetra

import (
	"sort"
	"strconv"
	"strings"
)

// ProcInfo is one process's live state, for the Monitoring "processes" tab.
type ProcInfo struct {
	PID     int
	Name    string
	State   string
	CPUPct  float64
	MemMiB  float64
	Threads int
}

// ProcSnapshot is the live process-table overview: aggregate counts by state
// plus a bounded top-N by CPU (falling back to top-N by mem on the very
// first tick, before ProcCPUCalc has a prior sample to diff against).
// Deliberately snapshot-only: per-process series would be a cardinality trap
// (hundreds of short-lived pids per host), so this is never fed into the
// SampleStore -- see collectProcesses/daemon.go's sampler loop.
type ProcSnapshot struct {
	Total, Running, Sleeping, Zombie int
	Top                              []ProcInfo
}

// procTopN bounds ProcSnapshot.Top: a live overview tab showing every
// process on a host with hundreds of them would be noise, not signal.
const procTopN = 15

// parseProcPidStat parses the content of one /proc/<pid>/stat file.
func parseProcPidStat(content string) (comm, state string, utime, stime uint64, numThreads int, rssPages int64, ok bool) {
	openIdx := strings.IndexByte(content, '(')
	closeIdx := strings.LastIndexByte(content, ')')
	if openIdx < 0 || closeIdx < 0 || closeIdx < openIdx {
		return "", "", 0, 0, 0, 0, false
	}
	comm = content[openIdx+1 : closeIdx]
	rest := strings.Fields(content[closeIdx+1:])
	const needFields = 22 // highest index used below (21) + 1
	if len(rest) < needFields {
		return "", "", 0, 0, 0, 0, false
	}
	state = rest[0]
	var err error
	if utime, err = strconv.ParseUint(rest[11], 10, 64); err != nil {
		return "", "", 0, 0, 0, 0, false
	}
	if stime, err = strconv.ParseUint(rest[12], 10, 64); err != nil {
		return "", "", 0, 0, 0, 0, false
	}
	if numThreads, err = strconv.Atoi(rest[17]); err != nil {
		return "", "", 0, 0, 0, 0, false
	}
	if rssPages, err = strconv.ParseInt(rest[21], 10, 64); err != nil {
		return "", "", 0, 0, 0, 0, false
	}
	return comm, state, utime, stime, numThreads, rssPages, true
}

// ProcCPUCalc turns cumulative per-process CPU jiffies (utime+stime from /proc/<pid>/stat)
// into a CPU% by diffing against the previous slow-tier sample.
type ProcCPUCalc struct {
	prev      map[int]uint64
	prevTotal uint64
}

// Pct computes CPUPct = (procJiffiesDelta / totalCPUDelta) * 100 for each pid in cur (pid
// -> utime+stime jiffies).
func (p *ProcCPUCalc) Pct(cur map[int]uint64, curTotal uint64) map[int]float64 {
	out := make(map[int]float64, len(cur))
	totalDelta := float64(curTotal) - float64(p.prevTotal)
	if p.prev != nil && totalDelta > 0 {
		for pid, j := range cur {
			pj, ok := p.prev[pid]
			if !ok || j < pj {
				continue // new pid this tick, or counter went backwards: leave at 0
			}
			out[pid] = float64(j-pj) / totalDelta * 100
		}
	}
	p.prev = cur
	p.prevTotal = curTotal
	return out
}

// collectProcesses gathers the live process-table overview from /proc/<pid>/stat entries:
// per-state counts and a bounded top-N by CPU%.
func collectProcesses(fs FileSource, cpucalc *ProcCPUCalc, pageSizeKB int) ProcSnapshot {
	var snap ProcSnapshot
	paths, _ := fs.Glob("/proc/[0-9]*/stat")

	type parsedProc struct {
		pid      int
		comm     string
		state    string
		jiffies  uint64
		threads  int
		rssPages int64
	}
	procs := make([]parsedProc, 0, len(paths))
	for _, p := range paths {
		pid, perr := strconv.Atoi(pidFromStatPath(p))
		if perr != nil {
			continue
		}
		b, err := fs.Read(p)
		if err != nil {
			continue // vanished mid-scan (or otherwise unreadable): skip, don't fail
		}
		comm, state, utime, stime, threads, rss, ok := parseProcPidStat(string(b))
		if !ok {
			continue // malformed/short stat line: skip
		}
		procs = append(procs, parsedProc{pid, comm, state, utime + stime, threads, rss})
		snap.Total++
		switch state {
		case "R":
			snap.Running++
		case "S", "D":
			snap.Sleeping++
		case "Z":
			snap.Zombie++
		}
	}

	// System-wide CPU total for the per-process % denominator: same file/ parser collectFast
	// uses for system CPU%, read independently here since this runs on its own.
	var curTotal uint64
	if b, err := fs.Read("/proc/stat"); err == nil {
		if st, err := parseProcStat(string(b)); err == nil {
			curTotal = st.Total
		}
	}
	cur := make(map[int]uint64, len(procs))
	for _, pr := range procs {
		cur[pr.pid] = pr.jiffies
	}
	pct := cpucalc.Pct(cur, curTotal)

	top := make([]ProcInfo, 0, len(procs))
	anyCPU := false
	for _, pr := range procs {
		cp := pct[pr.pid]
		if cp > 0 {
			anyCPU = true
		}
		top = append(top, ProcInfo{
			PID:     pr.pid,
			Name:    pr.comm,
			State:   pr.state,
			CPUPct:  cp,
			MemMiB:  float64(pr.rssPages) * float64(pageSizeKB) / 1024,
			Threads: pr.threads,
		})
	}
	if anyCPU {
		sort.Slice(top, func(i, j int) bool { return top[i].CPUPct > top[j].CPUPct })
	} else {
		sort.Slice(top, func(i, j int) bool { return top[i].MemMiB > top[j].MemMiB })
	}
	if len(top) > procTopN {
		top = top[:procTopN]
	}
	snap.Top = top
	return snap
}

// pidFromStatPath extracts the numeric pid directory component from a
// "/proc/<pid>/stat" glob match (e.g. "/proc/1234/stat" -> "1234").
func pidFromStatPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2]
}
