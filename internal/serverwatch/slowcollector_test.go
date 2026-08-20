package serverwatch

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlowCollectorNeverOverlaps pins the fix for the Critical race in Task 6:
// two slow collections must never run concurrently, or they would data-race on
// the single-owner calculators (smartCache/NetRateCalc/ProcCPUCalc maps). It
// parks the first collection open (blocked on a channel) past its deadline so
// runOnce returns while the collection is still in flight, then hammers runOnce
// concurrently and asserts collect is never entered more than once at a time.
//
// Without the in-flight guard in runOnce, each hammering call spawns its own
// collect goroutine, driving max concurrency well above 1 -> this test fails
// (and, in production, the unguarded map writes trip the race detector /
// "concurrent map read and map write"). With the guard, every hammering call
// CAS-fails and skips, so max concurrency stays 1.
func TestSlowCollectorNeverOverlaps(t *testing.T) {
	hub := &slowHub{}
	var lastSlow atomic.Int64
	var inCollect atomic.Int32
	var maxConcurrent atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})

	sc := &slowCollector{
		hub:             hub,
		lastSlowSuccess: &lastSlow,
		now:             func() int64 { return 1 },
		deadlineSec:     func() int64 { return 5 }, // ample margin for the guarded happy path
		collect: func() Snapshot {
			cur := inCollect.Add(1)
			for { // track the high-water mark of concurrent collect entries
				m := maxConcurrent.Load()
				if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
					break
				}
			}
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release // hold the collection open to force overlap attempts
			inCollect.Add(-1)
			return Snapshot{Online: true}
		},
	}

	// First collection: enters collect and parks on release. Its inner goroutine
	// keeps running, so inFlight stays set even after runOnce eventually returns.
	go sc.runOnce()
	<-entered

	// Hammer runOnce concurrently while the first collection is still parked.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc.runOnce()
		}()
	}
	// Give the hammering calls time to either skip (guard) or spawn a second
	// collect (no guard) before checking the high-water mark.
	time.Sleep(100 * time.Millisecond)
	if got := maxConcurrent.Load(); got > 1 {
		t.Fatalf("collect entered concurrently: max concurrency %d (want 1) -- in-flight guard missing/broken", got)
	}

	close(release) // let the first collection complete and publish
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, ok := hub.latest(); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, _, ok := hub.latest(); !ok {
		t.Fatal("first collection never published to hub")
	}
	if lastSlow.Load() != 1 {
		t.Fatalf("lastSlowSuccess = %d, want 1 (should advance on a successful publish)", lastSlow.Load())
	}
}

// TestSlowCollectorSlowButCompletingAdvancesLiveness pins the fix for the
// IMPORTANT finding: a collection that OVERRUNS its deadline but still completes
// must publish + advance lastSlowSuccess (so a merely-slow collector keeps the
// watchdog fed), even though runOnce already returned at the deadline for
// pacing. Against the old discard-on-timeout behavior lastSlowSuccess never
// advances here and this test fails.
func TestSlowCollectorSlowButCompletingAdvancesLiveness(t *testing.T) {
	hub := &slowHub{}
	var lastSlow atomic.Int64
	sc := &slowCollector{
		hub:             hub,
		lastSlowSuccess: &lastSlow,
		now:             func() int64 { return 42 },
		deadlineSec:     func() int64 { return 1 }, // 1s deadline
		collect: func() Snapshot {
			time.Sleep(1500 * time.Millisecond) // overruns the deadline, but completes
			return Snapshot{Online: true}
		},
	}

	start := time.Now()
	sc.runOnce()
	if elapsed := time.Since(start); elapsed > 1300*time.Millisecond {
		t.Fatalf("runOnce blocked %s past its ~1s deadline; it must return at the deadline for pacing", elapsed)
	}

	// The collect is still running; liveness advances only once it completes.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastSlow.Load() == 42 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lastSlow.Load() != 42 {
		t.Fatalf("lastSlowSuccess = %d, want 42: a slow-but-completing collect must still advance liveness", lastSlow.Load())
	}
	if _, _, ok := hub.latest(); !ok {
		t.Fatal("a slow-but-completing collect must still publish its snapshot to hub")
	}
}

// TestSlowCollectorWedgedDoesNotAdvanceLiveness pins the other half of the
// contract: a genuinely wedged collect (never returns) must NOT advance
// lastSlowSuccess, so the watchdog still trips after collectorStaleSec and
// systemd restarts a truly stuck daemon.
func TestSlowCollectorWedgedDoesNotAdvanceLiveness(t *testing.T) {
	hub := &slowHub{}
	var lastSlow atomic.Int64
	release := make(chan struct{})
	sc := &slowCollector{
		hub:             hub,
		lastSlowSuccess: &lastSlow,
		now:             func() int64 { return 7 },
		deadlineSec:     func() int64 { return 1 },
		collect: func() Snapshot {
			<-release // wedged until the test releases it
			return Snapshot{Online: true}
		},
	}

	sc.runOnce() // returns at ~1s deadline; collect still wedged
	time.Sleep(200 * time.Millisecond)
	if lastSlow.Load() != 0 {
		t.Fatalf("wedged collect advanced lastSlowSuccess to %d; the watchdog would never trip", lastSlow.Load())
	}
	if _, _, ok := hub.latest(); ok {
		t.Fatal("wedged collect must not publish")
	}

	// Release so the background goroutine drains before the test returns.
	close(release)
	drain := time.Now().Add(time.Second)
	for time.Now().Before(drain) {
		if _, _, ok := hub.latest(); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}
