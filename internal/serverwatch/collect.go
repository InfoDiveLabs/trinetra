package serverwatch

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type CPUStat struct{ Total, Idle uint64 }

func parseProcStat(s string) (CPUStat, error) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu ") && !strings.HasPrefix(line, "cpu\t") {
			continue
		}
		f := strings.Fields(line)[1:]
		var st CPUStat
		for i, v := range f {
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return st, err
			}
			st.Total += n
			if i == 3 || i == 4 { // idle + iowait
				st.Idle += n
			}
		}
		return st, nil
	}
	return CPUStat{}, fmt.Errorf("no cpu line in /proc/stat")
}

func cpuBusyPct(prev, cur CPUStat) float64 {
	dt := float64(cur.Total - prev.Total)
	di := float64(cur.Idle - prev.Idle)
	if dt <= 0 {
		return 0
	}
	return (dt - di) / dt * 100
}

type MemInfo struct{ TotalKB, AvailableKB, SwapTotalKB, SwapFreeKB uint64 }

func parseMeminfo(s string) (MemInfo, error) {
	var m MemInfo
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseUint(f[1], 10, 64)
		switch strings.TrimSuffix(f[0], ":") {
		case "MemTotal":
			m.TotalKB = n
		case "MemAvailable":
			m.AvailableKB = n
		case "SwapTotal":
			m.SwapTotalKB = n
		case "SwapFree":
			m.SwapFreeKB = n
		}
	}
	if m.TotalKB == 0 {
		return m, fmt.Errorf("no MemTotal")
	}
	return m, nil
}

func (m MemInfo) UsedPct() float64 {
	if m.TotalKB == 0 {
		return 0
	}
	return float64(m.TotalKB-m.AvailableKB) / float64(m.TotalKB) * 100
}

func (m MemInfo) SwapUsedPct() float64 {
	if m.SwapTotalKB == 0 {
		return 0
	}
	return float64(m.SwapTotalKB-m.SwapFreeKB) / float64(m.SwapTotalKB) * 100
}

func parseLoadavg(s string) (l1, l5, l15 float64, err error) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0, fmt.Errorf("bad loadavg %q", s)
	}
	if l1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return
	}
	if l5, err = strconv.ParseFloat(f[1], 64); err != nil {
		return
	}
	l15, err = strconv.ParseFloat(f[2], 64)
	return
}

func parseUptime(s string) (time.Duration, error) {
	f := strings.Fields(s)
	if len(f) < 1 {
		return 0, fmt.Errorf("bad uptime")
	}
	secs, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(secs * float64(time.Second)), nil
}

type DiskUsage struct {
	Mount                 string
	UsedPct               float64
	FreeBytes, TotalBytes uint64
}

// parseDF parses `df -PB1` output (POSIX 1-byte blocks).
func parseDF(s string) ([]DiskUsage, error) {
	var out []DiskUsage
	sc := bufio.NewScanner(strings.NewReader(s))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		total, _ := strconv.ParseUint(f[1], 10, 64)
		avail, _ := strconv.ParseUint(f[3], 10, 64)
		pct, _ := strconv.ParseFloat(strings.TrimSuffix(f[4], "%"), 64)
		out = append(out, DiskUsage{
			Mount: f[5], UsedPct: pct, FreeBytes: avail, TotalBytes: total,
		})
	}
	return out, nil
}

func parseThermal(s string) (float64, error) {
	milli, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return milli / 1000, nil
}
