// Package trinetra: eventbus_test.go pins eventBus's core contract --
// the whole reason it exists, per its own doc (eventbus.go): Publish must
// NEVER block the sampler loop / dispatchAndLog caller, regardless of how
// many subscribers there are or how slow/absent they are. Every test here
// is synchronous and single-goroutine (buffered channels make that
// possible), so `go test -race` is the only concurrency proof needed on top.
package trinetra

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestEventBusPublishNoSubscribersDoesNotBlock is the base case: Publish
// against a freshly built bus with zero subscribers must return immediately
// rather than blocking or panicking (there is, after all, nowhere to send
// to).
func TestEventBusPublishNoSubscribersDoesNotBlock(t *testing.T) {
	b := newEventBus()
	b.Publish(core.Event{Kind: "snapshot", Time: 1})
}

// TestEventBusPublishDeliversToAllSubscribers pins the fan-out: a single
// Publish call must reach every subscriber currently registered, each
// getting its own copy off its own channel.
func TestEventBusPublishDeliversToAllSubscribers(t *testing.T) {
	b := newEventBus()
	ch1, cancel1 := b.Subscribe()
	defer cancel1()
	ch2, cancel2 := b.Subscribe()
	defer cancel2()

	ev := core.Event{Kind: "alert_fire", Severity: "critical", Source: "anomaly", Title: "cpu high", Time: 100}
	b.Publish(ev)

	got1 := <-ch1
	if got1 != ev {
		t.Fatalf("ch1 got %+v, want %+v", got1, ev)
	}
	got2 := <-ch2
	if got2 != ev {
		t.Fatalf("ch2 got %+v, want %+v", got2, ev)
	}
}

// TestEventBusPublishDropsWhenSubscriberBufferFull pins the drop-on-full
// contract: filling a subscriber's buffer to capacity (eventBusBuffer) and
// then publishing one more event must not block the publisher -- the
// overflow event is simply dropped, and the events already buffered stay
// readable in order.
func TestEventBusPublishDropsWhenSubscriberBufferFull(t *testing.T) {
	b := newEventBus()
	ch, cancel := b.Subscribe()
	defer cancel()

	for i := 0; i < eventBusBuffer; i++ {
		b.Publish(core.Event{Kind: "snapshot", Time: int64(i)})
	}
	// The buffer is now exactly full (cap eventBusBuffer, 0 read out yet).
	// This next Publish must return immediately (select/default drop) rather
	// than block -- if it blocked, this test would hang and -race/the test
	// timeout would catch it.
	b.Publish(core.Event{Kind: "snapshot", Time: 999})

	for i := 0; i < eventBusBuffer; i++ {
		got := <-ch
		if got.Time != int64(i) {
			t.Fatalf("event %d: got Time=%d, want %d (the overflow event must have been dropped, not an earlier one)", i, got.Time, i)
		}
	}
	select {
	case extra := <-ch:
		t.Fatalf("channel had an extra buffered event %+v; want exactly %d, with the overflow dropped", extra, eventBusBuffer)
	default:
	}
}

// TestEventBusCancelRemovesSubscriberAndClosesChannel pins cancel's two
// effects: a Publish after cancel is no longer delivered to that
// subscriber (proven by the channel being closed, not just silent), and
// calling cancel a second time is safe (no panic on double-close).
func TestEventBusCancelRemovesSubscriberAndClosesChannel(t *testing.T) {
	b := newEventBus()
	ch, cancel := b.Subscribe()

	cancel()

	// A closed channel with nothing buffered receives the zero value with
	// ok==false immediately -- proving the channel was actually closed, not
	// just abandoned.
	if v, ok := <-ch; ok {
		t.Fatalf("ch open after cancel: got %+v, ok=%v, want closed", v, ok)
	}

	// A Publish after cancel must not somehow resurrect delivery to this
	// subscriber -- there is nothing left to observe on ch (it's closed),
	// this just proves Publish itself doesn't panic against a bus with no
	// remaining subscribers.
	b.Publish(core.Event{Kind: "snapshot", Time: 1})

	cancel() // double-cancel must be safe (no panic on double-close).
}
