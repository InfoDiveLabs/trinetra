package core

import (
	"fmt"
	"slices"
	"time"
)

// Occurrence is one concrete [Start, End) instant (unix seconds) that a
// Maintenance window's recurring definition expands to.
type Occurrence struct {
	Start, End int64
}

// occHHMM parses "HH:MM" into its hour/minute parts, ok=false if it doesn't
// even scan as two ints -- mirrors internal/trinetra/digest.go's parseHHMM
// (that package can't be imported here -- server.go's Deps doc: the module
// graph is deliberately one-way, internal/trinetra -> internal/web/
// internal/core, never back). A Maintenance saved through
// FleetAPI.SaveMaintenance has already had its From/To range-checked
// (0-23/0-59), so this only needs to agree on the same permissive scan, not
// re-validate.
func occHHMM(s string) (h, m int, ok bool) {
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	return h, m, true
}

// occWallDuration returns the elapsed clock time from fromH:fromM to
// toH:toM, wrapping past midnight (adding 24h) when crosses is true -- a
// plain duration derived from the two wall-clock readings, deliberately NOT
// tied to any specific calendar day or timezone offset (see
// MaintenanceOccurrences' doc for why this matters across a DST
// transition).
func occWallDuration(fromH, fromM, toH, toM int, crosses bool) time.Duration {
	diff := (toH*60 + toM) - (fromH*60 + fromM)
	if crosses {
		diff += 24 * 60
	}
	return time.Duration(diff) * time.Minute
}

// MaintenanceOccurrences returns every occurrence of m that overlaps
// [from, until), expanded in m's own TZ. m.From > m.To (lexicographically,
// which works for zero-padded HH:MM) means the window crosses midnight, so
// the occurrence's END is computed as start.Add(wallDuration) -- NOT via a
// second time.Date call on the following calendar day.
//
// This matters across a DST transition: building both ends independently
// with time.Date, then adding a calendar day for a midnight crossing, lets a
// fall-back transition (clocks set back an hour) silently double the
// occurrence's real elapsed duration -- e.g. a 01:00-02:00 window in
// America/New_York on 2024-11-03 would otherwise last 2 real hours, not 1,
// because "01:00" and "02:00" that day straddle the point where the clock
// repeats an hour. Adding a plain, timezone-agnostic wall-clock duration to
// an absolute start Time is immune to that: the occurrence always lasts
// exactly as long as its configured HH:MM difference says, regardless of any
// DST transition inside it.
//
// An unparsable From/To/TZ (should not happen -- validated at creation by
// SaveMaintenance) yields no occurrences rather than an error, since this is
// also called from the alert-suppression hot path (internal/trinetra's
// silenceStore.Suppressed/silencesForNode, via a thin wrapper that delegates
// to this exact function -- there is only one implementation, exported here
// so internal/web's fleet silences page can compute the SAME "next
// occurrence" without internal/web ever importing internal/trinetra), which
// must never fail loudly on bad data.
func MaintenanceOccurrences(m Maintenance, from, until time.Time) []Occurrence {
	loc, err := time.LoadLocation(m.TZ)
	if err != nil {
		return nil
	}
	fromH, fromM, ok1 := occHHMM(m.From)
	toH, toM, ok2 := occHHMM(m.To)
	if !ok1 || !ok2 {
		return nil
	}
	crosses := m.From > m.To
	dur := occWallDuration(fromH, fromM, toH, toM, crosses)
	if dur <= 0 {
		return nil
	}

	var out []Occurrence
	// Scan a day either side of the range too, so an occurrence that starts
	// the day before `from` (crossing midnight into the range) or starts
	// just before `until` is never missed.
	d := from.In(loc).AddDate(0, 0, -1)
	end := until.In(loc).AddDate(0, 0, 1)
	for !d.After(end) {
		// The occurrence's weekday is the weekday of its START: a Sunday
		// 22:00 -> Monday 02:00 window is owned by Sunday, the day being
		// scanned here, never by the Monday its End happens to land on.
		if slices.Contains(m.Weekdays, int(d.Weekday())) {
			y, mo, day := d.Date()
			occStart := time.Date(y, mo, day, fromH, fromM, 0, 0, loc)
			occEnd := occStart.Add(dur)
			if occEnd.After(from) && occStart.Before(until) {
				out = append(out, Occurrence{Start: occStart.Unix(), End: occEnd.Unix()})
			}
		}
		d = d.AddDate(0, 0, 1)
	}
	return out
}

// occurrenceScanWindow is how far past `from` NextMaintenanceOccurrence
// scans for a candidate: comfortably more than 7 days so every weekday is
// guaranteed at least one chance to fire even with MaintenanceOccurrences'
// own +/-1 day padding.
const occurrenceScanWindow = 8 * 24 * time.Hour

// NextMaintenanceOccurrence returns the earliest Occurrence of m that has
// not yet ENDED as of from (Occurrence.End > from.Unix()) -- this covers
// both a window currently active (Start <= from < End) and one still to
// come, whichever is sooner, which is what the fleet silences page's list
// means by a maintenance window's "next occurrence". ok is false only when
// m's Weekdays/From/To/TZ don't parse (should not happen for anything
// SaveMaintenance has already validated and stored).
func NextMaintenanceOccurrence(m Maintenance, from time.Time) (Occurrence, bool) {
	occs := MaintenanceOccurrences(m, from, from.Add(occurrenceScanWindow))
	var best Occurrence
	found := false
	for _, occ := range occs {
		if occ.End <= from.Unix() {
			continue
		}
		if !found || occ.Start < best.Start {
			best = occ
			found = true
		}
	}
	return best, found
}
