package trinetra

import (
	"sync/atomic"
	"time"
)

// slowCollector runs one slow-tier collection at a time, off the sampler loop, bounding
// each attempt with a deadline.
type slowCollector struct {
	hub             *slowHub
	lastSlowSuccess *atomic.Int64
	now             func() int64
	// collect runs one full slow collection (collectSlow + NetRates + Processes) and returns
	// the snapshot.
	collect func() Snapshot
	// deadlineSec is the per-attempt deadline in seconds (derived from SampleInterval by the
	// caller); re-read each runOnce so a config change is picked up on the next attempt.
	deadlineSec func() int64
	inFlight    atomic.Bool
}

// runOnce starts a collection unless one is already in flight, then waits up to the
// deadline for it PURELY FOR PACING.
func (sc *slowCollector) runOnce() {
	if !sc.inFlight.CompareAndSwap(false, true) {
		// A previous collection is still running (outlived its deadline); do NOT
		// start a second one -- it would race the first on the shared calculators.
		return
	}
	deadline := sc.deadlineSec()
	// Buffered so the goroutine can always signal completion and run its inFlight-clearing
	// defer even after runOnce has already returned on the deadline branch.
	done := make(chan struct{}, 1)
	go func() {
		defer sc.inFlight.Store(false)
		s := sc.collect()
		// Publish + advance liveness here, not in runOnce's select: this is the ONLY publisher
		// (no double-publish).
		sc.hub.publish(s)
		sc.lastSlowSuccess.Store(sc.now())
		done <- struct{}{}
	}()
	select {
	case <-done:
		// completed within the deadline
	case <-time.After(time.Duration(deadline) * time.Second):
		// slow/stall: return for pacing so the collector loop stays responsive.
	}
}
