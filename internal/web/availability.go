//go:build web

package web

import (
	"fmt"
	"time"
)

// availabilityWindow/availabilityBlockDur define the dashboard's Availability
// strip: the last 24h split into 15-min blocks (96 of them), mirroring the
// mockup demo's hardcoded N=96/mins=15 shape (internal/web/assets/app.js's
// now-removed #hbstrip builder) but computed from real downtime events
// instead of faked client-side.
const (
	availabilityWindow   = 24 * time.Hour
	availabilityBlockDur = 15 * time.Minute
)

// availabilityBlockCount is availabilityWindow/availabilityBlockDur (96).
var availabilityBlockCount = int(availabilityWindow / availabilityBlockDur)

// AvailabilityBlock is one 15-min slot of the 24h strip, oldest-first (index
// 0 is the block starting 24h ago; the last is the block ending "now").
type AvailabilityBlock struct {
	// Down is true when at least one downtime event overlaps this block.
	Down bool `json:"down"`
	// Label is the block's start time (server-local "HH:MM"), for the
	// strip's hover title.
	Label string `json:"label"`
}

// Availability is DashboardView's real replacement for the dashboard's old
// hardcoded #hbstrip demo (app.js's N=96/dF=68/dT=70/wA=41, "1 incident ·
// 45m" literal text): a 24h series of up/down 15-min blocks plus the summary
// figures the strip's header line shows, computed from the downtime
// EventsStore (the same Task 9 seam events_store.go exposes) rather than
// faked client-side. There is no "degraded" state here (the mockup's demo had
// one, backed by nothing real) -- a block is either up or down.
type Availability struct {
	// Blocks is availabilityBlockCount 15-min blocks, oldest first.
	Blocks []AvailabilityBlock `json:"blocks"`
	// UptimePct is (window - downtime) / window * 100, computed from each
	// event's EXACT overlap with the window (clipped to it), not
	// block-quantized -- so it doesn't round-trip through the 15-min
	// granularity the Blocks display uses.
	UptimePct float64 `json:"uptime_pct"`
	// Incidents is the number of distinct downtime events overlapping the
	// window. Each DownEventView already IS one incident (the daemon's
	// downtime tracker never merges separate outages), so this is simply
	// len(events), not a re-merged/deduplicated count.
	Incidents int `json:"incidents"`
	// IncidentsLabel is Incidents pluralized for the strip's header line
	// ("0 incidents", "1 incident", "2 incidents").
	IncidentsLabel string `json:"incidents_label"`
	// DowntimeStr is the total in-window downtime, human-formatted ("0m",
	// "45m", "1h 20m").
	DowntimeStr string `json:"downtime_str"`
}

// ComputeAvailability builds the last-24h Availability ending at now (Unix
// seconds) from store's downtime events. A nil store (Deps.Events unset --
// e.g. store-writes-disabled mode) or a query error both degrade to "no
// events" (100% up, 0 incidents), mirroring uptimePct30d's (handlers_alerts.
// go) and downtimeAPIHandler's existing tolerance for the same inputs.
func ComputeAvailability(store EventsStore, now int64) Availability {
	to := now
	from := to - int64(availabilityWindow/time.Second)

	var events []DownEventView
	if store != nil {
		if evs, err := store.Events(from, to); err == nil {
			events = evs
		}
	}

	blockDur := int64(availabilityBlockDur / time.Second)
	blocks := make([]AvailabilityBlock, availabilityBlockCount)
	for i := range blocks {
		bStart := from + int64(i)*blockDur
		bEnd := bStart + blockDur
		down := false
		for _, e := range events {
			if e.Start < bEnd && e.End > bStart {
				down = true
				break
			}
		}
		blocks[i] = AvailabilityBlock{Down: down, Label: time.Unix(bStart, 0).Format("15:04")}
	}

	var downSec int64
	for _, e := range events {
		s, en := e.Start, e.End
		if s < from {
			s = from
		}
		if en > to {
			en = to
		}
		if en > s {
			downSec += en - s
		}
	}

	total := to - from
	uptimePct := 100.0
	if total > 0 {
		uptimePct = 100 * (1 - float64(downSec)/float64(total))
		if uptimePct < 0 {
			uptimePct = 0
		}
	}

	return Availability{
		Blocks:         blocks,
		UptimePct:      uptimePct,
		Incidents:      len(events),
		IncidentsLabel: incidentsLabel(len(events)),
		DowntimeStr:    formatDowntime(time.Duration(downSec) * time.Second),
	}
}

// incidentsLabel pluralizes an incident count for the strip's header line.
func incidentsLabel(n int) string {
	if n == 1 {
		return "1 incident"
	}
	return fmt.Sprintf("%d incidents", n)
}

// formatDowntime renders a downtime duration the strip's header wants: "0m"
// for none, "45m" under an hour, "1h 20m" at/above -- mirroring app.js's
// client-side historyFmtDur for the analogous history-page downtime panel.
func formatDowntime(d time.Duration) string {
	d = d.Round(time.Minute)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
