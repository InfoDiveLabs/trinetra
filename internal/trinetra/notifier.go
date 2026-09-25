package trinetra

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Severity classifies how urgent an Alert is.
type Severity int

const (
	SevInfo Severity = iota
	SevWarning
	SevCritical
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevWarning:
		return "warning"
	case SevCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// ParseSeverity parses the string form produced by Severity.String back
// into a Severity value.
func ParseSeverity(s string) (Severity, error) {
	switch s {
	case "info":
		return SevInfo, nil
	case "warning":
		return SevWarning, nil
	case "critical":
		return SevCritical, nil
	default:
		return 0, fmt.Errorf("invalid severity %q", s)
	}
}

// Alert is a channel-agnostic notification payload. It is distinct from
// Event (anomaly.go): Event is the internal alert-state transition, Alert
// is what gets handed to a Notifier for delivery.
type Alert struct {
	Key      string
	Title    string
	Body     string
	Severity Severity
	Kind     string // "fire" | "recover"
	Source   string
	Time     int64
}

// Notifier delivers an Alert over some channel (Telegram, email, webhook, ...).
type Notifier interface {
	Name() string
	Send(ctx context.Context, a Alert) error
}

// DeliveryResult reports the outcome of dispatching an Alert to one channel.
type DeliveryResult struct {
	Channel string
	Err     error
}

// targetKind derives the general category an alert's key refers to, taking
// everything before the first ':' (e.g. "disk:/" -> "disk", "cpu" -> "cpu").
func targetKind(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return key
}

// Route describes the conditions under which a channel should receive an
// Alert.
type Route struct {
	// MinSeverity is the lowest Severity this route will let through.
	MinSeverity Severity
	// IncludeKinds, if non-empty, restricts delivery to alerts whose
	// target-kind (see targetKind) is in this list.
	IncludeKinds []string
	// ExcludeKinds blocks delivery for alerts whose target-kind is in this
	// list, regardless of IncludeKinds.
	ExcludeKinds []string
	// CriticalOverridesQuiet, when true, lets SevCritical alerts through
	// during quiet hours even though everything else is suppressed.
	CriticalOverridesQuiet bool
}

// Allows reports whether a should be delivered on this route, given whether
// quiet hours are currently active.
func (r Route) Allows(a Alert, quiet bool) bool {
	if a.Severity < r.MinSeverity {
		return false
	}

	kind := targetKind(a.Key)
	if len(r.IncludeKinds) > 0 && !containsString(r.IncludeKinds, kind) {
		return false
	}
	if containsString(r.ExcludeKinds, kind) {
		return false
	}

	if quiet {
		if a.Severity < SevCritical {
			return false
		}
		if !r.CriticalOverridesQuiet {
			return false
		}
	}

	return true
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// Channel pairs a Notifier with the Route that gates which alerts it
// receives.
type Channel struct {
	N       Notifier
	Route   Route
	Enabled bool
}

// Dispatcher fans an Alert out to a set of enabled, routing-matched Channels
// concurrently, bounding each delivery attempt with a timeout so one slow or
// broken channel can't hold up the others.
type Dispatcher struct {
	channels []Channel
	timeout  time.Duration
}

// NewDispatcher builds a Dispatcher that sends to channels, giving each
// Send call up to timeout to complete.
func NewDispatcher(channels []Channel, timeout time.Duration) *Dispatcher {
	return &Dispatcher{channels: channels, timeout: timeout}
}

// Dispatch sends a to every enabled channel whose Route allows it (given
// whether quiet hours are active), returning one DeliveryResult per channel
// actually attempted. It never panics: a Notifier.Send that panics is
// recovered and reported as an error.
//
// Dispatch itself returns within roughly d.timeout regardless of whether a
// given Notifier.Send honors its context: each send runs in its own
// goroutine and Dispatch races that goroutine's result against a local
// timer rather than blocking on it, so a Send that ignores ctx and hangs
// forever cannot delay Dispatch's return (though its goroutine will leak
// until the misbehaving call eventually completes -- cooperative
// cancellation via ctx remains the well-behaved path).
func (d *Dispatcher) Dispatch(a Alert, quiet bool) []DeliveryResult {
	var matched []Channel
	for _, c := range d.channels {
		if c.Enabled && c.Route.Allows(a, quiet) {
			matched = append(matched, c)
		}
	}

	results := make([]DeliveryResult, len(matched))

	var wg sync.WaitGroup
	for i, c := range matched {
		wg.Add(1)
		go func(i int, n Notifier) {
			defer wg.Done()
			results[i] = DeliveryResult{Channel: n.Name(), Err: d.collect(n, a)}
		}(i, c.N)
	}
	wg.Wait()

	return results
}

// collect runs n.Send in its own goroutine and races its result against a
// local timer, so it returns within roughly d.timeout even if n.Send
// ignores its context and never returns (the goroutine then leaks until
// that call eventually completes).
func (d *Dispatcher) collect(n Notifier, a Alert) error {
	done := make(chan error, 1)
	go func() { done <- sendSafely(n, a, d.timeout) }()

	select {
	case err := <-done:
		return err
	case <-time.After(d.timeout):
		return fmt.Errorf("notifier %s timed out after %s", n.Name(), d.timeout)
	}
}

// sendSafely calls n.Send under a timeout, recovering any panic and turning
// it into an error so a broken Notifier can never take down the caller.
func sendSafely(n Notifier, a Alert, timeout time.Duration) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("notifier %s panicked: %v", n.Name(), r)
		}
	}()

	return n.Send(ctx, a)
}

type queuedAlert struct {
	a     Alert
	quiet bool
}

// NotifierQueue decouples alert delivery from the caller: Enqueue is
// non-blocking and Run drains to the Dispatcher on its own goroutine. On a
// slow uplink the queue bounds memory by dropping the OLDEST non-critical
// alert; critical and recover alerts are never dropped.
type NotifierQueue struct {
	mu      sync.Mutex
	items   []queuedAlert
	cap     int
	dropped atomic.Int64
	wake    chan struct{}
	disp    atomic.Pointer[Dispatcher]
}

func NewNotifierQueue(d *Dispatcher, capacity int) *NotifierQueue {
	if capacity < 1 {
		capacity = 1
	}
	q := &NotifierQueue{cap: capacity, wake: make(chan struct{}, 1)}
	q.disp.Store(d)
	return q
}

func (q *NotifierQueue) SetDispatcher(d *Dispatcher) { q.disp.Store(d) }
func (q *NotifierQueue) Dropped() int64              { return q.dropped.Load() }

func undroppable(a Alert) bool { return a.Severity >= SevCritical || a.Kind == "recover" }

func (q *NotifierQueue) Enqueue(a Alert, quiet bool) {
	q.mu.Lock()
	if len(q.items) >= q.cap {
		if i := q.indexOfOldestDroppable(); i >= 0 {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.dropped.Add(1)
		} else if !undroppable(a) {
			// queue is all-undroppable and full, and the newcomer is droppable:
			// drop the newcomer rather than an alert we promised to keep.
			q.mu.Unlock()
			q.dropped.Add(1)
			return
		}
		// else: newcomer is undroppable and queue is all-undroppable -> allow
		// growth past cap (bounded by reality: critical bursts are rare).
	}
	q.items = append(q.items, queuedAlert{a: a, quiet: quiet})
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *NotifierQueue) indexOfOldestDroppable() int {
	for i := range q.items {
		if !undroppable(q.items[i].a) {
			return i
		}
	}
	return -1
}

func (q *NotifierQueue) pop() (queuedAlert, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return queuedAlert{}, false
	}
	it := q.items[0]
	q.items = q.items[1:]
	return it, true
}

func (q *NotifierQueue) Run(ctx context.Context) {
	for {
		it, ok := q.pop()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-q.wake:
				continue
			}
		}
		if d := q.disp.Load(); d != nil {
			d.Dispatch(it.a, it.quiet)
		}
	}
}

// snapshotKeysForTest exposes the queued keys for tests only.
func (q *NotifierQueue) snapshotKeysForTest() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	ks := make([]string, len(q.items))
	for i := range q.items {
		ks[i] = q.items[i].a.Key
	}
	return ks
}
