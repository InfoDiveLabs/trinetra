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

// occHHMM parses "HH:MM" into hour/minute, ok=false if it does not scan as two ints.
func occHHMM(s string) (h, m int, ok bool) {
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	return h, m, true
}

// occWallDuration returns the clock time from fromH:fromM to toH:toM, adding 24h when
// crosses is true.
func occWallDuration(fromH, fromM, toH, toM int, crosses bool) time.Duration {
	diff := (toH*60 + toM) - (fromH*60 + fromM)
	if crosses {
		diff += 24 * 60
	}
	return time.Duration(diff) * time.Minute
}

// MaintenanceOccurrences returns every occurrence of m overlapping [from, until), expanded
// in m's own TZ. m.From > m.To (lexicographic.
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
	// Scan a day either side of the range so an occurrence crossing midnight into
	// the range, or starting just before `until`, is not missed.
	d := from.In(loc).AddDate(0, 0, -1)
	end := until.In(loc).AddDate(0, 0, 1)
	for !d.After(end) {
		// The weekday is that of the occurrence's START: Sunday 22:00 to Monday 02:00 is
		// owned by Sunday.
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

// occurrenceScanWindow is how far past `from` NextMaintenanceOccurrence scans:
// more than 7 days so every weekday gets a chance despite the +/-1 day padding.
const occurrenceScanWindow = 8 * 24 * time.Hour

// NextMaintenanceOccurrence returns the earliest Occurrence of m that has not
// ENDED as of from, covering both an active window and an upcoming one. ok is
// false only when m's Weekdays/From/To/TZ do not parse.
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
