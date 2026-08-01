package serverwatch

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

// parseProcPidStat parses the content of one /proc/<pid>/stat file. The comm
// field (2nd whitespace-separated field per proc(5)) is wrapped in parens
// and may itself contain spaces and/or parens (e.g. "(sd-pam)"-style kernel
// thread names, or a process renamed to include punctuation), so it cannot
// be split positionally by whitespace like the rest of the line: this locates
// the FIRST '(' and the LAST ')' to bound comm, then splits whatever follows
// the closing paren by whitespace to recover the remaining fields.
//
// Per proc(5), the 1-indexed field positions are 3=state, 14=utime,
// 15=stime, 20=num_threads, 24=rss (in pages). Fields 1 (pid) and 2 (comm)
// are consumed above, so in the post-comm remainder (0-indexed) those become
// state=rest[0], utime=rest[11], stime=rest[12], num_threads=rest[17],
// rss=rest[21]. Returns ok=false for anything malformed or too short to
// contain all of these -- callers should skip that pid rather than trust
// zero-valued output.
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

// ProcCPUCalc turns cumulative per-process CPU jiffies (utime+stime from
// /proc/<pid>/stat) into a CPU% by diffing against the previous slow-tier
// sample, mirroring NetRateCalc's stateful prev-delta pattern (net.go). It
// is meant to be owned by a single goroutine (the sampler loop) and called
// once per slow tick -- no locking, since only one goroutine ever touches it.
type ProcCPUCalc struct {
	prev      map[int]uint64
	prevTotal uint64
}

// Pct computes CPUPct = (procJiffiesDelta / totalCPUDelta) * 100 for each
// pid in cur (pid -> utime+stime jiffies), against the previous sample.
// curTotal is the current /proc/stat CPUStat.Total jiffies (all CPUs, all
// states) -- the same system-wide denominator cpuBusyPct uses, just applied
// per-process here.
//
// The very first call ever (no prior sample) has nothing to diff against,
// so every pid is absent from the result (reads as 0 via the zero value).
// totalDelta<=0 (clock didn't advance, or a /proc/stat counter went
// backwards) also yields 0 for every pid that tick, rather than a
// divide-by-zero or a misleading spike. A pid absent from the previous
// sample (spawned since the last tick) likewise gets 0 for this tick: its
// jiffies-since-birth would overstate a single-tick percentage, so it's
// deferred to the next tick, where a proper delta exists. prev/prevTotal are
// updated to cur/curTotal for the next call regardless of any of the above.
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

// collectProcesses gathers the live process-table overview from
// /proc/<pid>/stat entries: per-state counts and a bounded top-N by CPU%
// (falling back to top-N by mem when every process reads 0% CPU -- i.e. the
// very first slow tick, before cpucalc has a prior sample to diff against).
// pageSizeKB converts /proc/<pid>/stat's rss (in pages) to MiB.
//
// Robust to processes disappearing mid-scan, a normal race on any real
// host: a Read error or a malformed/short stat line for a given pid just
// skips that pid rather than failing the whole snapshot.
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

	// System-wide CPU total for the per-process % denominator: same file/
	// parser collectFast uses for system CPU%, read independently here since
	// this runs on its own (slow-tier) cadence.
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
