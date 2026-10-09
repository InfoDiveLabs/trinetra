package web

import "github.com/InfoDiveLabs/trinetra/internal/core"

// DownEventView moved to internal/core.
type DownEventView = core.DownEventView

// EventsStore is this package's own minimal seam onto the daemon's downtime event log:
// Events returns the downtime events overlapping [from, to].
type EventsStore interface {
	// Events returns downtime events overlapping [from, to] (Unix seconds).
	Events(from, to int64) ([]DownEventView, error)
}
