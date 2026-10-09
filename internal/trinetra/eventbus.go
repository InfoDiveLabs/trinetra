// Package trinetra: eventbus.go implements the daemon's in-process live event fan-out
// (issue-tracked as the A2 "live push" epic): the sampler loop and enqueueAndLog.
package trinetra

import (
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// eventBusBuffer is the per-subscriber channel capacity Subscribe allocates.
const eventBusBuffer = 64

// eventBus fans out core.Event values to any number of subscribers without ever blocking
// the publisher: a subscriber whose buffer is full drops the event.
type eventBus struct {
	mu   sync.Mutex
	subs map[int]chan core.Event
	next int
}

// newEventBus builds an empty eventBus ready for Publish/Subscribe.
func newEventBus() *eventBus {
	return &eventBus{subs: make(map[int]chan core.Event)}
}

// Publish delivers ev to every subscriber currently registered, never blocking: each
// subscriber's channel is sent to under select/default.
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

// Subscribe registers a new buffered subscriber channel (capacity eventBusBuffer) and
// returns it plus a cancel func that unregisters it and closes the channel.
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
