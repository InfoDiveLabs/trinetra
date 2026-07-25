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

// buildDigest renders a digest message from precomputed stats: peakCPU/
// peakMem (already the max over whatever window the caller queried),
// sampleCount (how many points backed those peaks), and downs (the downtime
// events overlapping the window). Precomputed rather than raw []Sample so
// callers can source the numbers from a SampleStore query (digestNow) without
// this function knowing anything about storage.
func buildDigest(title, window string, peakCPU, peakMem float64, sampleCount int, downs []DownEvent) string {
	var totalDown int64
	for _, d := range downs {
		totalDown += d.DurationSec
	}
	var b strings.Builder
	b.WriteString(title)
	fmt.Fprintf(&b, "\nsamples: %d", sampleCount)
	fmt.Fprintf(&b, "\npeak CPU: %.0f%%  peak mem: %.0f%%", peakCPU, peakMem)
	fmt.Fprintf(&b, "\ndowntime (%s): %s across %d events", window, humanDur(totalDown), len(downs))
	return b.String()
}

func buildDailyDigest(peakCPU, peakMem float64, sampleCount int, downs []DownEvent) string {
	return buildDigest("📊 daily digest", "24h", peakCPU, peakMem, sampleCount, downs)
}
