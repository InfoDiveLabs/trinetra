// Package trinetra: fleet_dispatch.go is the master alerting engine's keyed
// dispatcher (B3 review round 2): replaces a bare goroutine-per-alert with
// one FIFO lane per dispatch key (so a recover is never run before its own
// fire) and a global semaphore bounding how many dispatches run at once.
// Enqueue never blocks its caller.
package trinetra

import "sync"

// dispatchConcurrency bounds how many dispatch jobs (fleetAlertEngine's
// deliverAndReceipt calls) run at once, across every key.
const dispatchConcurrency = 8

// dispatchLane is one key's FIFO queue and whether a pump goroutine is
// currently draining it.
type dispatchLane struct {
	mu      sync.Mutex
	queue   []func()
	pumping bool
}

// keyedDispatcher runs jobs off their own goroutine(s), never the caller's:
// each key gets a FIFO (so ordering within a key -- e.g. a fire then its
// recover -- is preserved regardless of how long an earlier job takes), and
// a global semaphore of dispatchConcurrency bounds total concurrency across
// all keys. Enqueue itself never blocks: it only appends to a mutex-guarded
// slice and, at most, starts one goroutine for a lane that was idle.
type keyedDispatcher struct {
	sem chan struct{}

	mu    sync.Mutex
	lanes map[string]*dispatchLane

	// wg tracks every job that has been enqueued but not yet run, purely so
	// a test can deterministically drain the dispatcher (waitIdleForTest)
	// instead of racing it.
	wg sync.WaitGroup
}

func newKeyedDispatcher() *keyedDispatcher {
	return &keyedDispatcher{sem: make(chan struct{}, dispatchConcurrency), lanes: map[string]*dispatchLane{}}
}

// Enqueue appends job to key's FIFO. If key's lane is currently idle, this
// starts exactly one goroutine to drain it (and every lane that becomes
// idle again exits its goroutine, so a quiet dispatcher holds none); if a
// pump for key is already running, job is simply appended for it to pick up
// next -- never a second goroutine per key.
func (d *keyedDispatcher) Enqueue(key string, job func()) {
	d.mu.Lock()
	l, ok := d.lanes[key]
	if !ok {
		l = &dispatchLane{}
		d.lanes[key] = l
	}
	d.mu.Unlock()

	l.mu.Lock()
	l.queue = append(l.queue, job)
	start := !l.pumping
	if start {
		l.pumping = true
	}
	l.mu.Unlock()

	d.wg.Add(1)
	if start {
		go d.pump(l)
	}
}

// pump drains l's queue strictly in order, one job at a time, until it is
// empty, acquiring the dispatcher's global semaphore around each job so at
// most dispatchConcurrency run concurrently across every lane. It exits
// (clearing l.pumping) as soon as it observes an empty queue; Enqueue's own
// check-then-set against the SAME l.mu is what guarantees no job is ever
// left stranded in that race (see Enqueue's doc comment).
func (d *keyedDispatcher) pump(l *dispatchLane) {
	for {
		l.mu.Lock()
		if len(l.queue) == 0 {
			l.pumping = false
			l.mu.Unlock()
			return
		}
		job := l.queue[0]
		l.queue = l.queue[1:]
		l.mu.Unlock()

		d.sem <- struct{}{}
		runJob(job)
		<-d.sem

		d.wg.Done()
	}
}

// runJob calls job with its own panic recovery, so one broken alert
// delivery can never crash the dispatcher's long-lived pump goroutine or
// leak its semaphore slot.
func runJob(job func()) {
	defer func() { _ = recover() }()
	job()
}

// waitIdleForTest blocks until every job Enqueue has accepted so far has
// actually run. Test-only.
func (d *keyedDispatcher) waitIdleForTest() { d.wg.Wait() }
