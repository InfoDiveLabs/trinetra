//go:build web

package web

import "serverwatch/internal/core"

// SeriesPoint moved to internal/core (core.API contract task 1). This is a
// Go type alias, not a new type, so every existing handler/template
// reference in this package keeps compiling unchanged. See
// core.SeriesPoint's doc for the field semantics (TS is Unix seconds; Min/
// Avg/Max are the same value for a raw point, a real rollup for a
// downsampled one).
type SeriesPoint = core.SeriesPoint

// SeriesStore is this package's own minimal seam onto the daemon's history
// storage: Query returns metric's points over [from, to] (inclusive), Unix
// seconds, ordered by TS ascending.
//
// This is the Task 9 (#65) resolution of the Task 1 placeholder that made
// Deps.Store `any` (see server.go's earlier doc note) — the same pattern
// Task 8 (#64) used for Deps.Snapshot/DashboardView. internal/web must never
// import internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go), so it cannot reference
// serverwatch.SampleStore or serverwatch.Resolution directly: this interface
// is internal/web's own shape, and internal/serverwatch/daemon_web.go's
// adapter (seriesStoreAdapter) is what wraps the real SampleStore to satisfy
// it — resolving raw-vs-1m resolution via serverwatch.PickResolution
// internally, so this package never needs to know a Resolution type exists.
//
// A nil SeriesStore is valid (Deps.Store may be nil, e.g. store-writes-
// disabled mode or a daemon that hasn't been given one): callers
// (seriesAPIHandler) must treat a nil Deps.Store as "no data" — an empty
// series, not a panic — rather than assuming this interface is always set.
type SeriesStore interface {
	// Query returns metric's points within [from, to] (Unix seconds,
	// inclusive), ordered by TS ascending. An unrecognized metric name is
	// NOT an error: it returns an empty (possibly nil) slice and a nil
	// error, exactly like querying a metric that simply has no data yet.
	Query(metric string, from, to int64) ([]SeriesPoint, error)
}
