package serverwatch

import (
	"strconv"
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

func reconstructPowerDown(lastBeat, boot time.Time, interval time.Duration) (DownEvent, bool) {
	if !boot.After(lastBeat) {
		return DownEvent{}, false // clock skew / no gap
	}
	gap := boot.Sub(lastBeat)
	if gap <= 2*interval {
		return DownEvent{}, false
	}
	return DownEvent{
		Type:        "power_down",
		Start:       lastBeat.Unix(),
		End:         boot.Unix(),
		DurationSec: int64(gap.Seconds()),
	}, true
}

// NetTracker records live internet-down intervals while the process is running.
type NetTracker struct {
	downSince int64 // 0 = currently online
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
