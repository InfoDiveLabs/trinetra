package serverwatch

import (
	"fmt"
	"strings"
	"time"
)

func parseHHMM(spec string) (h, m int, ok bool) {
	var hh, mm int
	if _, err := fmt.Sscanf(spec, "%d:%d", &hh, &mm); err != nil {
		return 0, 0, false
	}
	return hh, mm, true
}

func matchDaily(spec string, now, lastRun time.Time) bool {
	if spec == "" {
		return false
	}
	h, m, ok := parseHHMM(spec)
	if !ok {
		return false
	}
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	if now.Before(target) {
		return false
	}
	return lastRun.Before(target)
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func matchWeekly(spec string, now, lastRun time.Time) bool {
	parts := strings.SplitN(spec, "@", 2)
	if len(parts) != 2 {
		return false
	}
	wd, ok := weekdays[strings.ToLower(parts[0])]
	if !ok || now.Weekday() != wd {
		return false
	}
	return matchDaily(parts[1], now, lastRun)
}

func buildDigest(title, window string, samples []Sample, downs []DownEvent) string {
	var peakCPU, peakMem float64
	for _, s := range samples {
		if s.CPU > peakCPU {
			peakCPU = s.CPU
		}
		if s.MemPct > peakMem {
			peakMem = s.MemPct
		}
	}
	var totalDown int64
	for _, d := range downs {
		totalDown += d.DurationSec
	}
	var b strings.Builder
	b.WriteString(title)
	fmt.Fprintf(&b, "\nsamples: %d", len(samples))
	fmt.Fprintf(&b, "\npeak CPU: %.0f%%  peak mem: %.0f%%", peakCPU, peakMem)
	fmt.Fprintf(&b, "\ndowntime (%s): %s across %d events", window, humanDur(totalDown), len(downs))
	return b.String()
}

func buildDailyDigest(samples []Sample, downs []DownEvent) string {
	return buildDigest("📊 daily digest", "24h", samples, downs)
}
