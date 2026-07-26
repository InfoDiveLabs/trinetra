//go:build web

package web

// DownEventView is internal/web's own projection of one downtime event —
// exactly the fields templates/history.html's "Downtime · 30d" timeline/rows
// (and the /api/downtime JSON response, handlers_history.go) need, expressed
// with no serverwatch import. It mirrors serverwatch.DownEvent
// field-for-field (Type is "power_down"/"net_down"; Start/End are Unix
// seconds; DurationSec is the event's length).
type DownEventView struct {
	Type        string `json:"type"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DurationSec int64  `json:"duration_sec"`
}

// EventsStore is this package's own minimal seam onto the daemon's downtime
// event log: Events returns the downtime events overlapping [from, to]
// (Unix seconds). It is the downtime counterpart to SeriesStore
// (series_store.go) — the same Task 9 (#65) pattern: internal/web must never
// import internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go), so it cannot reference
// serverwatch.SampleStore or serverwatch.DownEvent directly. This interface
// is internal/web's own shape, and internal/serverwatch/daemon_web.go's
// adapter (seriesStoreAdapter, which satisfies BOTH SeriesStore and
// EventsStore) is what wraps the real SampleStore to satisfy it.
//
// A nil EventsStore is valid (Deps.Events may be nil, e.g. store-writes-
// disabled mode): callers (downtimeAPIHandler) must treat a nil Deps.Events
// as "no events" — an empty list, not a panic.
type EventsStore interface {
	// Events returns downtime events overlapping [from, to] (Unix seconds).
	// A range with no events is not an error: it returns an empty (possibly
	// nil) slice and a nil error.
	Events(from, to int64) ([]DownEventView, error)
}
