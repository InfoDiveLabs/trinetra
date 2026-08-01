//go:build web

package web

import "serverwatch/internal/core"

// AvailabilityBlock, Availability, and ComputeAvailability moved to
// internal/core/availability.go (core.API contract task 2). Availability is
// a Go type alias (not a new type), so every existing handler/template
// reference in this package (v.Availability.Blocks, etc.) keeps compiling
// unchanged.
//
// ComputeAvailability now delegates to core.ComputeAvailability: this
// package's EventsStore (events_store.go) satisfies core.EventsSource
// structurally (same Events(from, to int64) ([]DownEventView, error)
// method, and DownEventView is itself a core alias), so an EventsStore value
// is directly assignable to the core.EventsSource parameter with no adapter
// needed.
type AvailabilityBlock = core.AvailabilityBlock
type Availability = core.Availability

// ComputeAvailability builds the last-24h Availability ending at now (Unix
// seconds) from store's downtime events. A nil store (Deps.Events unset --
// e.g. store-writes-disabled mode) or a query error both degrade to "no
// events" (100% up, 0 incidents), mirroring uptimePct30d's (handlers_alerts.
// go) and downtimeAPIHandler's existing tolerance for the same inputs. See
// core.ComputeAvailability (internal/core/availability.go) for the full
// implementation.
func ComputeAvailability(store EventsStore, now int64) Availability {
	return core.ComputeAvailability(store, now)
}
