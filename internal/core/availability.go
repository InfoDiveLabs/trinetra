package core

import (
	"fmt"
	"sort"
	"time"
)

// coalesceDownEvents merges overlapping or touching events of the SAME type into
// single windows, so adjacent outages read as one incident and count their UNION,
// not a double-counted sum, toward downtime and uptime % (#116). Input need not be
// sorted; the result is sorted by Start.
func coalesceDownEvents(evs []DownEventView) []DownEventView {
	if len(evs) < 2 {
		return evs
	}
	sorted := make([]DownEventView, len(evs))
	copy(sorted, evs)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].Type < sorted[j].Type
	})
	out := make([]DownEventView, 0, len(sorted))
	for _, e := range sorted {
		if n := len(out); n > 0 && out[n-1].Type == e.Type && e.Start <= out[n-1].End {
			if e.End > out[n-1].End {
				out[n-1].End = e.End
				out[n-1].DurationSec = out[n-1].End - out[n-1].Start
			}
			continue
		}
		out = append(out, e)
	}
	return out
}

// availabilityWindow/availabilityBlockDur define the dashboard Availability
// strip: the last 24h in 15-min blocks (96).
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

// Availability is the dashboard's 24h strip: up/down 15-min blocks plus the
// header summary, computed from the downtime EventsSource. A block is either up
// or down; there is no degraded state.
type Availability struct {
	// Blocks is availabilityBlockCount 15-min blocks, oldest first.
	Blocks []AvailabilityBlock `json:"blocks"`
	// UptimePct is (window - downtime) / window * 100 from each event's exact overlap
	// with the window, not block-quantized.
	UptimePct float64 `json:"uptime_pct"`
	// Incidents is the number of downtime events overlapping the window. Each
	// DownEventView is already one incident (the tracker never merges outages), so
	// this is len(events).
	Incidents int `json:"incidents"`
	// IncidentsLabel is Incidents pluralized for the strip's header line
	// ("0 incidents", "1 incident", "2 incidents").
	IncidentsLabel string `json:"incidents_label"`
	// DowntimeStr is the total in-window downtime, human-formatted ("0m",
	// "45m", "1h 20m").
	DowntimeStr string `json:"downtime_str"`
}

// EventsSource is the seam ComputeAvailability needs onto a downtime event log:
// Events returns events overlapping [from, to] (Unix seconds). internal/web's
// EventsStore satisfies it without importing back into internal/web.
//
// A range with no events is not an error: it returns an empty (possibly
// nil) slice and a nil error.
type EventsSource interface {
	Events(from, to int64) ([]DownEventView, error)
}

// ComputeAvailability builds the last-24h Availability ending at now (Unix
// seconds). A nil events or a query error degrades to "no events" (100% up, 0
// incidents).
func ComputeAvailability(events EventsSource, now int64) Availability {
	to := now
	from := to - int64(availabilityWindow/time.Second)

	var evs []DownEventView
	if events != nil {
		if e, err := events.Events(from, to); err == nil {
			evs = coalesceDownEvents(e)
		}
	}

	blockDur := int64(availabilityBlockDur / time.Second)
	blocks := make([]AvailabilityBlock, availabilityBlockCount)
	for i := range blocks {
		bStart := from + int64(i)*blockDur
		bEnd := bStart + blockDur
		down := false
		for _, e := range evs {
			if e.Start < bEnd && e.End > bStart {
				down = true
				break
			}
		}
		blocks[i] = AvailabilityBlock{Down: down, Label: time.Unix(bStart, 0).Format("15:04")}
	}

	var downSec int64
	for _, e := range evs {
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
		Incidents:      len(evs),
		IncidentsLabel: incidentsLabel(len(evs)),
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

// formatDowntime renders "0m" for none, "45m" under an hour, "1h 20m" at/above.
func formatDowntime(d time.Duration) string {
	d = d.Round(time.Minute)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
