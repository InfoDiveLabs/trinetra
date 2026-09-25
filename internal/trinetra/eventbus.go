// Package serverwatch: eventbus.go implements the daemon's in-process live
// event fan-out (issue-tracked as the A2 "live push" epic): the sampler
// loop and enqueueAndLog (daemon.go) both PUBLISH core.Event values on
// every snapshot tick / dispatched alert, and inprocAPI.Subscribe
// (coreapi_inproc.go) hands each control-socket subscriber its own
// SUBSCRIPTION onto the same stream. This file only ever touches sync +
// internal/core (no third-party import), so it never breaks the default
// build's stdlib-only guarantee (TestDefaultBuildIsStdlibOnly,
// buildtag_test.go).
package trinetra

import (
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// eventBusBuffer is the per-subscriber channel capacity Subscribe
// allocates. It is deliberately small and finite: a subscriber that falls
// behind (a slow/stalled control-socket stream, or simply no reader at all
// between Subscribe and the first drain) drops events past this many
// buffered rather than growing without bound or blocking the publisher --
// see Publish's doc. Snapshot ticks are coalescable (the next one supersedes
// a dropped one) and alerts are also durably recorded in the alert log
// (enqueueAndLog, daemon.go), so a drop here is never the only record of
// what happened.
const eventBusBuffer = 64

// eventBus fans out core.Event values to any number of subscribers without
// ever blocking the publisher: a subscriber whose buffer is full drops the
// event (snapshot ticks are coalescable; alerts are also in the alert log).
// The zero value is not usable -- construct with newEventBus.
type eventBus struct {
	mu   sync.Mutex
	subs map[int]chan core.Event
	next int
}

// newEventBus builds an empty eventBus ready for Publish/Subscribe.
func newEventBus() *eventBus {
	return &eventBus{subs: make(map[int]chan core.Event)}
}

// Publish delivers ev to every subscriber currently registered, never
// blocking: each subscriber's channel is sent to under select/default, so a
// full buffer just drops ev for that one subscriber rather than stalling
// this call (and, transitively, the sampler loop / enqueueAndLog caller
// that invoked it) waiting for a reader. A nil bus is a safe no-op, mirroring
// this package's other nil-degrades-gracefully dependencies (e.g. alog in
// enqueueAndLog) -- callers that construct an inprocAPI without a live
// daemon bus (most existing tests) never need to thread one through here
// just to call code paths that happen to publish.
func (b *eventBus) Publish(ev core.Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// subscriber's buffer is full -- drop ev for it, keep going.
		}
	}
}

// Subscribe registers a new buffered subscriber channel (capacity
// eventBusBuffer) and returns it plus a cancel func that unregisters it and
// closes the channel. Cancel is idempotent (safe to call more than once --
// e.g. once from a ctx.Done() goroutine and once from an explicit caller
// path racing it); only the first call has any effect. Publish never
// observes a half-removed subscriber: both the map delete and the close
// happen while holding mu, and Publish holds the same lock while ranging
// subs, so a subscriber is either fully present (Publish may deliver to it)
// or fully gone (Publish's range simply won't see it) -- never a closed
// channel Publish is still trying to send on.
func (b *eventBus) Subscribe() (<-chan core.Event, func()) {
	b.mu.Lock()
	id := b.next
	b.next++
	ch := make(chan core.Event, eventBusBuffer)
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}
