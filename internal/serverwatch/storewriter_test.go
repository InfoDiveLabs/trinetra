package serverwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// blockingStore is a SampleStore whose Prune blocks until released, simulating
// the real tsFileStore's fsync stalling under a disk backlog while holding the
// store lock. appends counts how many Append calls landed.
type blockingStore struct {
	release <-chan struct{}
	appends atomic.Int64
	pruned  atomic.Int64
}

func (b *blockingStore) Append(ts int64, m MetricSet) error { b.appends.Add(1); return nil }
func (b *blockingStore) AppendEvent(e DownEvent) error      { return nil }
func (b *blockingStore) Query(metric string, from, to int64, res Resolution) ([]Point, error) {
	return nil, nil
}
func (b *blockingStore) Events(from, to int64) ([]DownEvent, error) { return nil, nil }
func (b *blockingStore) Downsample(nowUnix int64) error             { return nil }
func (b *blockingStore) Prune(nowUnix int64) error {
	<-b.release // block like a stuck fsync holding the store lock
	b.pruned.Add(1)
	return nil
}
func (b *blockingStore) Close() error               { return nil }
func (b *blockingStore) Stats() (int, int64, error) { return 0, 0, nil }

// The sampler must never block on storage I/O. With the writer wedged in a
// hung Prune, submit() must keep returning immediately (dropping batches),
// never stalling the caller -- otherwise a slow disk freezes liveness and the
// watchdog crash-loops the daemon (the real-world bug this guards against).
func TestStoreWriterSubmitNeverBlocksWhenPruneHangs(t *testing.T) {
	release := make(chan struct{})
	bs := &blockingStore{release: release}
	w := newStoreWriter(bs, 2)

	ctx, cancel := context.WithCancel(context.Background())
	go w.run(ctx)

	// First batch triggers maintenance -> writer enters Prune and blocks there.
	w.submit(storeWrite{ts: 1, sets: []MetricSet{{"cpu": 1}}, maintain: true, maintainNow: 1})

	// Give the writer a moment to pick up that batch and wedge in Prune.
	time.Sleep(50 * time.Millisecond)

	// Now hammer submit while the writer is stuck. Every call MUST return fast;
	// if submit blocked, this loop would take ~seconds and time out the test.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			w.submit(storeWrite{ts: int64(i), sets: []MetricSet{{"cpu": float64(i)}}})
		}
		close(done)
	}()

	select {
	case <-done:
		// good: submit stayed non-blocking despite the wedged Prune.
	case <-time.After(2 * time.Second):
		t.Fatal("submit blocked while the store was stuck in Prune -- the sampler would freeze")
	}

	if w.Dropped() == 0 {
		t.Fatal("expected dropped batches while the writer was wedged, got 0")
	}

	// Unblock and shut down cleanly.
	close(release)
	cancel()
}

// A healthy writer actually persists what it's given.
func TestStoreWriterAppendsAndMaintains(t *testing.T) {
	release := make(chan struct{})
	close(release) // never blocks
	bs := &blockingStore{release: release}
	w := newStoreWriter(bs, 8)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)

	w.submit(storeWrite{ts: 1, sets: []MetricSet{{"cpu": 1}, {"mem": 2}}, maintain: true, maintainNow: 1})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bs.appends.Load() == 2 && bs.pruned.Load() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("writer did not persist: appends=%d pruned=%d", bs.appends.Load(), bs.pruned.Load())
}
