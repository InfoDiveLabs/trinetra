package web

import "github.com/InfoDiveLabs/trinetra/internal/core"

// AvailabilityBlock, Availability, and ComputeAvailability moved to
// internal/core/availability.go.
type AvailabilityBlock = core.AvailabilityBlock
type Availability = core.Availability

// ComputeAvailability builds the last-24h Availability ending at now (Unix seconds) from
// store's downtime events.
func ComputeAvailability(store EventsStore, now int64) Availability {
	return core.ComputeAvailability(store, now)
}
