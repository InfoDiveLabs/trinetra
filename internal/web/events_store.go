//go:build web

package web

import "serverwatch/internal/core"

// DownEventView moved to internal/core (core.API contract task 1). This is a
// Go type alias, not a new type, so every existing handler/template
// reference in this package keeps compiling unchanged. See
// core.DownEventView's doc for the field semantics (Type is "power_down"/
// "net_down"; Start/End are Unix seconds; DurationSec is the event's
// length).
type DownEventView = core.DownEventView

// EventsStore is this package's own minimal seam onto the daemon's downtime
// event log: Events returns the downtime events overlapping [from, to]
// (Unix seconds). This is the Task 9 (#65) pattern: internal/web must never
// import internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go), so it cannot reference
// serverwatch.SampleStore or serverwatch.DownEvent directly. This interface
// is internal/web's own shape, and internal/serverwatch/daemon_web.go's
// adapter (seriesStoreAdapter) is what wraps the real SampleStore to
// satisfy it.
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
