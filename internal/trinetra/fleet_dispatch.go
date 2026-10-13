// Package trinetra: fleet_dispatch.go is the master alerting engine's keyed dispatcher:
// replaces a bare goroutine-per-alert with one FIFO lane per dispatch key.
package trinetra

import (
	"sync"
	"time"
)

// dispatchConcurrency bounds how many dispatch jobs (fleetAlertEngine's
// deliverAndReceipt calls) run at once, across every key.
const dispatchConcurrency = 8

// dispatchLane is one key's FIFO queue and whether a pump goroutine is currently draining
// it.
type dispatchLane struct {
	queue   []func()
	pumping bool
}

// keyedDispatcher runs jobs off their own goroutine(s), never the caller's: each key gets a
// FIFO.
type keyedDispatcher struct {
	sem chan struct{}

	// mu guards both lanes AND every dispatchLane's own fields (queue, pumping) -- a
	// dispatchLane has no mutex of its own.
	mu    sync.Mutex
	lanes map[string]*dispatchLane

	// stopped, once true (Stop), makes Enqueue silently drop new jobs instead of queuing them.
	stopped bool

	// wg tracks every job that has been enqueued but not yet run, purely so a test (or Stop)
	// can deterministically wait for the dispatcher to drain instead of racing it.
	wg sync.WaitGroup
}

func newKeyedDispatcher() *keyedDispatcher {
	return &keyedDispatcher{sem: make(chan struct{}, dispatchConcurrency), lanes: map[string]*dispatchLane{}}
}

// Enqueue appends job to key's FIFO and, if key's lane was idle (or didn't exist), starts
// exactly one goroutine to drain it.
var enqueueAfterUnlockHook func()

func (d *keyedDispatcher) Enqueue(key string, job func()) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	l, ok := d.lanes[key]
	if !ok {
		l = &dispatchLane{}
		d.lanes[key] = l
	}
	l.queue = append(l.queue, job)
	start := !l.pumping
	if start {
		l.pumping = true
	}
	// Count the job before releasing mu: once it is on the queue a running pump can dequeue,
	// run and Done() it immediately, so an Add after Unlock could drive the counter negative.
	d.wg.Add(1)
	d.mu.Unlock()
	if enqueueAfterUnlockHook != nil {
		enqueueAfterUnlockHook()
	}

	if start {
		go d.pump(key, l)
	}
}

// pump drains l's queue strictly in order, one job at a time, acquiring the dispatcher's
// global semaphore around each job.
func (d *keyedDispatcher) pump(key string, l *dispatchLane) {
	for {
		d.mu.Lock()
		if len(l.queue) == 0 {
			l.pumping = false
			delete(d.lanes, key)
			d.mu.Unlock()
			return
		}
		job := l.queue[0]
		l.queue = l.queue[1:]
		d.mu.Unlock()

		d.sem <- struct{}{}
		runJob(job)
		<-d.sem

		d.wg.Done()
	}
}

// runJob calls job with its own panic recovery, so one broken alert delivery can never
// crash the dispatcher's long-lived pump goroutine or leak its semaphore slot.
func runJob(job func()) {
	defer func() { _ = recover() }()
	job()
}

// waitIdleForTest blocks until every job Enqueue has accepted so far has actually run.
func (d *keyedDispatcher) waitIdleForTest() { d.wg.Wait() }

// laneCountForTest reports how many lanes are currently tracked (idle lanes
// are removed by pump, so a fully drained dispatcher reports 0). Test-only.
func (d *keyedDispatcher) laneCountForTest() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.lanes)
}

// Stop stops accepting new jobs (every Enqueue call after this is a no-op) and waits up to
// timeout for every already-queued or in-flight job to finish.
func (d *keyedDispatcher) Stop(timeout time.Duration) bool {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
