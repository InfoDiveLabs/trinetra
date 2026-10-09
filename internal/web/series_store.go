package web

import "github.com/InfoDiveLabs/trinetra/internal/core"

// SeriesPoint moved to internal/core. This is a Go type alias, not a new type,
// so every existing handler/template reference in this package keeps compiling
// unchanged. See core.SeriesPoint's doc for the field semantics (TS is Unix
// seconds; Min/Avg/Max are the same value for a raw point, a real rollup for a
// downsampled one).
type SeriesPoint = core.SeriesPoint
