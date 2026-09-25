package serverwatch

import (
	"context"
	"sync/atomic"
)

// storeWrite is one cycle's batch of SampleStore work: the metric sets and
// downtime events to append, plus whether to run the (fsync-heavy) Downsample
// + Prune maintenance afterwards.
type storeWrite struct {
	ts          int64
	sets        []MetricSet
	events      []DownEvent
	maintain    bool
	maintainNow int64
}

// storeWriter owns EVERY write to the SampleStore, off the sampler/liveness
// goroutine. This exists because the tsFileStore's maintenance (Prune/
// Downsample) fsyncs each rewritten series file while holding the store's
// lock, so on a large backlog or a slow/stuck disk that lock -- and thus a
// naive inline store.Append/Prune on the sampler -- can block for far longer
// than the systemd watchdog window, freezing liveness and crash-looping the
// daemon. By funnelling all store writes through this one goroutine and
// having the sampler submit() non-blockingly, a slow disk only delays sample
// persistence; it can never stall the sampler loop. Samples are best-effort:
// when the writer falls behind (submit's buffer full) the batch is dropped and
// counted rather than blocking the caller.
type storeWriter struct {
	store   SampleStore
	ch      chan storeWrite
	dropped atomic.Int64
	tee     atomic.Pointer[storeTee]
}

// storeTee receives every sample set and downtime event AFTER it has been
// written to the local store. The fleet child uses it to spool telemetry for
// the master (fleet_child.go); nil (the default, and always on solo) means
// no tee.
type storeTee interface {
	Samples(ts int64, ms MetricSet)
	Event(e DownEvent)
}

// setTee installs t; safe to call while run is active.
func (w *storeWriter) setTee(t storeTee) { w.tee.Store(&t) }

// newStoreWriter builds a storeWriter over store with a bounded submit buffer.
// capacity should be small: it only smooths brief writer lag, and dropping
// under sustained disk stress is the intended behaviour.
func newStoreWriter(store SampleStore, capacity int) *storeWriter {
	if capacity < 1 {
		capacity = 1
	}
	return &storeWriter{store: store, ch: make(chan storeWrite, capacity)}
}

// Dropped reports how many batches were dropped because the writer could not
// keep up (a sustained-slow-disk signal worth surfacing).
func (w *storeWriter) Dropped() int64 { return w.dropped.Load() }

// submit hands one cycle's batch to the writer WITHOUT blocking. If the writer
// is behind (blocked in a slow Append/Prune), the batch is dropped and counted
// rather than stalling the caller -- this is the whole point: the sampler must
// never wait on storage I/O.
func (w *storeWriter) submit(sw storeWrite) {
	select {
	case w.ch <- sw:
	default:
		w.dropped.Add(1)
	}
}

// run drains submitted batches until ctx is done, appending samples/events and
// running maintenance. All the potentially-blocking store I/O (including the
// fsync in Prune/Downsample) happens here, never on the caller's goroutine.
func (w *storeWriter) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sw := <-w.ch:
			var tee storeTee
			if p := w.tee.Load(); p != nil {
				tee = *p
			}
			for _, ms := range sw.sets {
				if len(ms) > 0 {
					if err := w.store.Append(sw.ts, ms); err == nil && tee != nil {
						tee.Samples(sw.ts, ms)
					}
				}
			}
			for i := range sw.events {
				if err := w.store.AppendEvent(sw.events[i]); err == nil && tee != nil {
					tee.Event(sw.events[i])
				}
			}
			if sw.maintain {
				_ = w.store.Downsample(sw.maintainNow)
				_ = w.store.Prune(sw.maintainNow)
			}
		}
	}
}
