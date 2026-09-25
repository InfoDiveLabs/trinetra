package trinetra

import (
	"strconv"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

const helpText = `commands:
/stats -- current readings
/status -- same as /stats
/disk -- filesystem usage
/net -- connectivity
/history [days] -- downtime history (default 7)
/down -- recent downtime events
/docker -- container states
/services -- failed systemd units
/help -- this message`

// handleCommand dispatches an inbound Telegram command to its renderer. c is
// the live config (thresholds for renderStatus's ok/warn/crit markers); it
// may be nil (renderStatus degrades to config.Default() rather than
// panicking), mirroring the existing nil-store degrade pattern below.
func handleCommand(text string, store SampleStore, snap Snapshot, c *config.Config) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return helpText
	}
	switch fields[0] {
	case "/stats", "/status":
		return renderStatus(snap, c)
	case "/disk":
		return renderDisks(snap.Disks, snap.DiskDetail)
	case "/net":
		return "internet: " + onlineStr(snap.Online)
	case "/history", "/down":
		days := 7
		if len(fields) > 1 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				days = n
			}
		}
		// Guard nil store: if OpenStore failed at daemon startup, degrade to
		// an empty history reply rather than a nil-pointer panic.
		if store == nil {
			return formatDowntimeList(nil)
		}
		since := time.Unix(snap.TS, 0).AddDate(0, 0, -days).Unix()
		evs, err := store.Events(since, snap.TS)
		if err != nil {
			return "error reading downtime: " + err.Error()
		}
		return formatDowntimeList(evs)
	case "/docker":
		return renderDocker(snap.Containers)
	case "/services":
		return renderServices(snap.FailedUnits)
	case "/help":
		return helpText
	default:
		return helpText
	}
}

// inQuietHours parses "HH-HH" (start-end, may wrap midnight). Empty disables.
func inQuietHours(spec string, now time.Time) bool {
	if spec == "" {
		return false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return false
	}
	start, err1 := strconv.Atoi(parts[0])
	end, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	h := now.Hour()
	if start <= end {
		return h >= start && h < end
	}
	return h >= start || h < end // wraps midnight
}
