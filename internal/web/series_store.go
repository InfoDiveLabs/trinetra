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
