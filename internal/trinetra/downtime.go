package trinetra

import (
	"strconv"
	"strings"
	"time"
)

type DownEvent struct {
	Type        string `json:"type"` // power_down | net_down
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}

func writeHeartbeat(path string, now time.Time) error {
	return writeFileAtomic(path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o644)
}

func readHeartbeat(path string, fs FileSource) (time.Time, bool) {
	b, err := fs.Read(path)
	if err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(string(trimSpace(b)), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(n, 0), true
}

// shouldReportDowntime suppresses a boot report whose window was already
// reported on a previous start (ev.End <= lastReportedEnd), so a restart that
// recomputes the same gap from an unchanged heartbeat does not re-notify.
func shouldReportDowntime(ev DownEvent, lastReportedEnd int64) bool {
	return ev.End > lastReportedEnd
}

func writeCleanStop(path string, now time.Time) error {
	return writeFileAtomic(path, []byte(strconv.FormatInt(now.Unix(), 10)), 0o644)
}

func readCleanStop(path string, fs FileSource) (int64, bool) {
	b, err := fs.Read(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(string(trimSpace(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// hostBootTime reads the host's boot time from /proc/stat's "btime <seconds>"
// line. ok is false when /proc/stat cannot be read or has no btime (e.g. a
// non-Linux dev box), so callers fall back rather than assume.
func hostBootTime(fs FileSource) (time.Time, bool) {
	b, err := fs.Read("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return time.Time{}, false
		}
		sec, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(sec, 0), true
	}
	return time.Time{}, false
}

// reconstructPowerDown decides whether the heartbeat gap between lastBeat and
// daemonStart is real HOST downtime worth recording (#116). A gap is host
// downtime only if the host actually rebooted during it: hostBoot lands after
// the last heartbeat, so the box was down from lastBeat until it came back. If
// the host stayed up the whole time (hostBoot at or before lastBeat), the gap
// is a MONITOR restart (crash loop, deploy, `systemctl restart`), NOT host
// downtime, and nothing is recorded -- otherwise a restart storm fabricates
// hours of "downtime" and tanks the uptime % even though the host never went
// down.
//
// hostBootOK reports whether the host boot time could be read. When it could
// not (non-Linux, unreadable /proc), we fall back to the pre-#116 behavior of
// treating a long gap as a power_down: better to over-report than to silently
// drop a real outage we cannot classify.
func reconstructPowerDown(lastBeat, daemonStart, hostBoot time.Time, hostBootOK bool, interval time.Duration) (DownEvent, bool) {
	if !daemonStart.After(lastBeat) {
		return DownEvent{}, false // clock skew / no gap
	}
	if daemonStart.Sub(lastBeat) <= 2*interval {
		return DownEvent{}, false
	}
	if hostBootOK && !hostBoot.After(lastBeat) {
		// Host was up across the whole gap: a monitoring gap, not host downtime.
		return DownEvent{}, false
	}
	// Real host downtime. It ended when the host booted (the true recovery
	// instant, when known and inside the gap), else when the daemon returned.
	end := daemonStart
	if hostBootOK && hostBoot.After(lastBeat) && hostBoot.Before(daemonStart) {
		end = hostBoot
	}
	return DownEvent{
		Type:        "power_down",
		Start:       lastBeat.Unix(),
		End:         end.Unix(),
		DurationSec: int64(end.Sub(lastBeat) / time.Second),
	}, true
}

// NetTracker records live internet-down intervals while the process is running.
type NetTracker struct {
	downSince int64 // 0 = currently online; sentinel assumes real epoch-second timestamps (never 0 in practice)
}

func (n *NetTracker) Update(online bool, now int64) (DownEvent, bool) {
	if online {
		if n.downSince == 0 {
			return DownEvent{}, false
		}
		ev := DownEvent{
			Type:        "net_down",
			Start:       n.downSince,
			End:         now,
			DurationSec: now - n.downSince,
		}
		n.downSince = 0
		return ev, true
	}
	if n.downSince == 0 {
		n.downSince = now
	}
	return DownEvent{}, false
}
