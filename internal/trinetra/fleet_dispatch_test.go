package trinetra

import (
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestKeyedDispatcherPreservesOrderPerKey covers the B3 review round 2
// ruling: a fire and its later recover on the SAME key are always run in
// order, even when the fire's own job is slow -- the recover must wait for
// it, never race ahead.
func TestKeyedDispatcherPreservesOrderPerKey(t *testing.T) {
	d := newKeyedDispatcher()
	var mu sync.Mutex
	var order []string

	d.Enqueue("n1:cpu", func() {
		time.Sleep(50 * time.Millisecond) // the "fire" on a slow channel
		mu.Lock()
		order = append(order, "fire")
		mu.Unlock()
	})
	d.Enqueue("n1:cpu", func() {
		mu.Lock()
		order = append(order, "recover")
		mu.Unlock()
	})
	d.waitIdleForTest()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "fire" || order[1] != "recover" {
		t.Fatalf("order = %v, want [fire recover]", order)
	}
}

// TestKeyedDispatcherDifferentKeysRunConcurrently is the flip side: two
// DIFFERENT keys must not serialize behind each other the way same-key jobs
// do.
func TestKeyedDispatcherDifferentKeysRunConcurrently(t *testing.T) {
	d := newKeyedDispatcher()
	release := make(chan struct{})
	started := make(chan struct{}, 2)

	d.Enqueue("n1:cpu", func() {
		started <- struct{}{}
		<-release
	})
	d.Enqueue("n2:cpu", func() {
		started <- struct{}{}
		<-release
	})

	// Both must start without either finishing first -- if they were
	// serialized, the second would never signal "started" until release is
	// closed, and this would time out.
	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("different keys did not run concurrently")
		}
	}
	close(release)
	d.waitIdleForTest()
}

// TestKeyedDispatcherBoundsGlobalConcurrency is the B3 review round 2
// ruling: 200 concurrent Enqueue calls across many different keys must
// never exceed dispatchConcurrency (8) jobs actually running at once.
func TestKeyedDispatcherBoundsGlobalConcurrency(t *testing.T) {
	d := newKeyedDispatcher()

	var mu sync.Mutex
	current, max := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		// A distinct key per job, so the global semaphore -- not the
		// per-key FIFO (which would trivially cap a single key's own
		// concurrency at 1 regardless) -- is what's actually exercised.
		key := keyFor(i)
		go func(key string) {
			defer wg.Done()
			d.Enqueue(key, func() {
				mu.Lock()
				current++
				if current > max {
					max = current
				}
				mu.Unlock()

				time.Sleep(2 * time.Millisecond)

				mu.Lock()
				current--
				mu.Unlock()
			})
		}(key)
	}
	wg.Wait() // every Enqueue call has returned (it never blocks)
	d.waitIdleForTest()

	mu.Lock()
	defer mu.Unlock()
	if max > dispatchConcurrency {
		t.Fatalf("observed %d concurrent dispatches, want <= %d", max, dispatchConcurrency)
	}
	if max < 2 {
		t.Fatalf("observed only %d concurrent dispatch(es); test is not exercising real concurrency", max)
	}
}

func keyFor(i int) string {
	// 200 distinct keys, one lane each, so every job is independently
	// eligible to run concurrently (bounded only by the global semaphore).
	b := make([]byte, 0, 8)
	for n := i; ; n /= 26 {
		b = append(b, byte('a'+n%26))
		if n < 26 {
			break
		}
	}
	return string(b)
}

// TestKeyedDispatcherEnqueueNeverBlocks covers "waiting must never block
// Submit's callers": Enqueue must return immediately even while a job for
// the SAME key is already running (and blocked).
func TestKeyedDispatcherEnqueueNeverBlocks(t *testing.T) {
	d := newKeyedDispatcher()
	blocking := make(chan struct{})
	d.Enqueue("n1:cpu", func() { <-blocking })

	done := make(chan struct{})
	go func() {
		d.Enqueue("n1:cpu", func() {})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked while an earlier job on the same key was still running")
	}
	close(blocking)
	d.waitIdleForTest()
}

// TestKeyedDispatcherRetiresDrainedLanes is the B3 review round 3 minor 1:
// a lane whose queue drains to empty must be removed from d.lanes, not held
// forever, so a long-lived dispatcher with high key churn doesn't leak one
// lane object per key it has ever seen.
func TestKeyedDispatcherRetiresDrainedLanes(t *testing.T) {
	d := newKeyedDispatcher()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		key := keyFor(i)
		go func(key string) {
			defer wg.Done()
			d.Enqueue(key, func() {})
		}(key)
	}
	wg.Wait()
	d.waitIdleForTest()

	// waitIdleForTest only guarantees every job RAN; pump's own lane
	// deletion happens immediately after, in the same loop iteration that
	// observed the queue empty, so poll briefly rather than assuming it has
	// already happened by the time wg.Wait() returns.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := d.laneCountForTest(); n == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("lanes = %d, want 0 after draining", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestKeyedDispatcherStopRejectsNewWorkAndBoundsWait is the B3 review round
// 3 minor 2: Stop returns promptly (bounded by its own timeout) even while
// a job is still blocked, work enqueued after Stop is dropped rather than
// queued forever, and no goroutine is left running once everything that WAS
// queued before Stop actually finishes.
func TestKeyedDispatcherStopRejectsNewWorkAndBoundsWait(t *testing.T) {
	before := runtime.NumGoroutine()

	d := newKeyedDispatcher()
	var mu sync.Mutex
	ran := 0
	block := make(chan struct{})
	d.Enqueue("k1", func() {
		<-block // still in flight when Stop is called
		mu.Lock()
		ran++
		mu.Unlock()
	})
	d.Enqueue("k1", func() { mu.Lock(); ran++; mu.Unlock() }) // queued behind it

	stopped := make(chan bool, 1)
	go func() { stopped <- d.Stop(50 * time.Millisecond) }()

	select {
	case ok := <-stopped:
		if ok {
			t.Fatal("Stop reported everything drained, but a job is still deliberately blocked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within roughly its own timeout")
	}

	// Enqueue after Stop must be a no-op, not queued forever.
	d.Enqueue("k2", func() { mu.Lock(); ran++; mu.Unlock() })

	close(block) // let the two pre-Stop jobs actually finish
	d.waitIdleForTest()

	mu.Lock()
	got := ran
	mu.Unlock()
	if got != 2 {
		t.Fatalf("ran = %d, want 2 (the two jobs queued before Stop; k2 must have been dropped)", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if n := d.laneCountForTest(); n == 0 {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("lanes = %d, want 0 once everything has drained", n)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let Stop's own wait-goroutine exit
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines = %d, want <= %d (baseline) -- leak after Stop drained", after, before)
	}
}
