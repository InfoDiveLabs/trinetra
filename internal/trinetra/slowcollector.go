package trinetra

import (
	"sync/atomic"
	"time"
)

// slowCollector runs one slow-tier collection at a time, off the sampler loop,
// bounding each attempt with a deadline. The single-owner, unsynchronized
// calculators collect closes over (smartCache/NetRateCalc/ProcCPUCalc) mean two
// overlapping collections would data-race on their maps, so runOnce guards
// against overlap with an in-flight flag: if a previous collection is still
// running (it outlived its deadline and the sampler moved on), the next tick is
// SKIPPED rather than starting a second concurrent collect. That preserves the
// deadline/stall semantics -- lastSlowSuccess stays un-advanced while a collect
// hangs, so the watchdog still trips after collectorStaleSec -- while
// guaranteeing collect is never entered concurrently.
type slowCollector struct {
	hub             *slowHub
	lastSlowSuccess *atomic.Int64
	now             func() int64
	// collect runs one full slow collection (collectSlow + NetRates + Processes)
	// and returns the snapshot. It owns the stateful calculators; runOnce's
	// in-flight guard is what makes that single-ownership safe across ticks.
	collect func() Snapshot
	// deadlineSec is the per-attempt deadline in seconds (derived from
	// SampleInterval by the caller); re-read each runOnce so a config change is
	// picked up on the next attempt.
	deadlineSec func() int64
	inFlight    atomic.Bool
}

// runOnce starts a collection unless one is already in flight, then waits up to
// the deadline for it PURELY FOR PACING. Publishing to hub and advancing
// lastSlowSuccess happen in the background collect goroutine itself, the moment
// collect returns -- so a slow-but-completing collection (one that overruns its
// deadline yet still finishes) still refreshes hub and advances liveness rather
// than being discarded. That matters because otherwise a merely-slow collector
// would never advance lastSlowSuccess, and after collectorStaleSec the watchdog
// would stop pinging and systemd would restart -- the exact restart loop this
// design prevents. A genuinely WEDGED collect (never returns) never reaches the
// publish/store, so lastSlowSuccess stays un-advanced and the watchdog still
// trips. On the deadline branch runOnce returns while the collection keeps
// running (inFlight stays set, so the next runOnce is skipped until it
// finishes) and the sampler's SlowStale reflects "no new data yet" until the
// goroutine publishes.
func (sc *slowCollector) runOnce() {
	if !sc.inFlight.CompareAndSwap(false, true) {
		// A previous collection is still running (outlived its deadline); do NOT
		// start a second one -- it would race the first on the shared calculators.
		return
	}
	deadline := sc.deadlineSec()
	// Buffered so the goroutine can always signal completion and run its
	// inFlight-clearing defer even after runOnce has already returned on the
	// deadline branch (nobody is receiving then).
	done := make(chan struct{}, 1)
	go func() {
		defer sc.inFlight.Store(false)
		s := sc.collect()
		// Publish + advance liveness here, not in runOnce's select: this is the
		// ONLY publisher (no double-publish), and it runs even when runOnce has
		// already returned at the deadline, so a slow-but-successful collect is
		// never discarded. hub (atomic.Pointer) and lastSlowSuccess (atomic) are
		// safe to touch from this goroutine.
		sc.hub.publish(s)
		sc.lastSlowSuccess.Store(sc.now())
		done <- struct{}{}
	}()
	select {
	case <-done:
		// completed within the deadline
	case <-time.After(time.Duration(deadline) * time.Second):
		// slow/stall: return for pacing so the collector loop stays responsive.
		// The goroutine publishes + advances liveness when it finishes (and keeps
		// inFlight set until then, so no overlapping collection can start).
	}
}
