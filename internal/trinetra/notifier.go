package trinetra

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/telegram"
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
	// Buttons, when non-nil, is an inline keyboard the telegram
	// Notifier attaches to this Alert's message (SendMessageWithButtons):
	// the master's alerting engine sets it on an incident's fire
	// notification only. Every other Notifier ignores it -- this is a
	// generic, channel-agnostic field so a future channel could use it too,
	// not a Telegram-specific one.
	Buttons [][]telegram.Button
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
	return d.dispatchMatched(a, matched)
}

// DispatchTo is Dispatch narrowed to a specific channel-name subset (fleet
// routing/escalation, task 5): a channel is sent to only if it is enabled,
// its own Route still Allows a (quiet hours/severity/kind gating is never
// bypassed by routing), AND either names contains the literal "*" or its
// Name() is in names. names with neither "*" nor any matching name delivers
// to nothing (an empty result), which is a valid outcome (e.g. a policy step
// naming a channel that was since removed from config).
func (d *Dispatcher) DispatchTo(a Alert, quiet bool, names []string) []DeliveryResult {
	all := false
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "*" {
			all = true
			continue
		}
		set[n] = true
	}
	var matched []Channel
	for _, c := range d.channels {
		if !c.Enabled || !c.Route.Allows(a, quiet) {
			continue
		}
		if !all && !set[c.N.Name()] {
			continue
		}
		matched = append(matched, c)
	}
	return d.dispatchMatched(a, matched)
}

// dispatchMatched fans a out to every channel in matched concurrently,
// bounding each by d.timeout -- the shared tail of Dispatch and DispatchTo,
// which differ only in how they build matched.
func (d *Dispatcher) dispatchMatched(a Alert, matched []Channel) []DeliveryResult {
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

// notifierMaxAttempts/notifierUndroppableMaxAttempts and
// notifierRetryWindow/notifierUndroppableRetryWindow bound how many times,
// and for how long, NotifierQueue.Run retries a channel that failed to
// deliver an alert before giving up and counting it permanently failed
// (final-review engine I1/ruling (a)). Undroppable alerts (critical, or a
// recover -- see undroppable) get the longer budget, since they already
// bypass the capacity-drop path in Enqueue and so are the ones this
// guarantee matters most for. Package vars, not consts, so a test can
// shrink them instead of actually waiting out a real 30 minutes.
var (
	notifierMaxAttempts            = 6
	notifierUndroppableMaxAttempts = 10
	notifierRetryWindow            = 30 * time.Minute
	notifierUndroppableRetryWindow = 2 * time.Hour
	// notifierBaseDelay/notifierMaxDelay set notifierBackoff's shape: delay
	// doubles from notifierBaseDelay each attempt, capped at
	// notifierMaxDelay. Package vars for the same test-speed reason as
	// above.
	notifierBaseDelay = 10 * time.Second
	notifierMaxDelay  = 5 * time.Minute
)

// notifierBackoff returns the delay before retrying a channel for the
// (1-based) attempt'th time it has failed: notifierBaseDelay * 2^(attempt-1),
// capped at notifierMaxDelay.
func notifierBackoff(attempt int) time.Duration {
	d := notifierBaseDelay
	for i := 1; i < attempt; i++ {
		if d >= notifierMaxDelay {
			return notifierMaxDelay
		}
		d *= 2
	}
	if d > notifierMaxDelay {
		d = notifierMaxDelay
	}
	return d
}

type queuedAlert struct {
	a     Alert
	quiet bool
	// attempt counts how many delivery attempts this alert has already had
	// (0 before the first). firstEnqueued is fixed at Enqueue time and never
	// changes across retries -- it anchors the retry window
	// (notifierRetryWindow/notifierUndroppableRetryWindow).
	attempt       int
	firstEnqueued time.Time
	// retryAt gates when popDue will hand this item back out again: the
	// zero value (a brand-new item from Enqueue) is always due immediately.
	retryAt time.Time
	// targetChannels is nil for a first attempt (deliver to every
	// enabled+routed channel, i.e. Dispatch's normal behavior) or the exact
	// set of channels that failed last time (DispatchTo) -- a channel that
	// already succeeded is structurally excluded from every later attempt,
	// so no channel ever receives the same alert twice.
	targetChannels []string
}

// NotifierQueue decouples alert delivery from the caller: Enqueue is
// non-blocking and Run drains to the Dispatcher on its own goroutine. On a
// slow uplink the queue bounds memory by dropping the OLDEST non-critical
// alert; critical and recover alerts are never dropped. A channel that
// fails is retried with backoff (see notifierBackoff) for up to
// notifierMaxAttempts/notifierRetryWindow (longer for an undroppable
// alert), rather than being attempted exactly once and then forgotten.
type NotifierQueue struct {
	mu      sync.Mutex
	items   []queuedAlert
	cap     int
	dropped atomic.Int64
	wake    chan struct{}
	disp    atomic.Pointer[Dispatcher]
	now     func() time.Time
	// permanentlyFailed counts alerts that exhausted their retry budget
	// with at least one channel still failing (Dropped, by contrast, counts
	// alerts evicted by the capacity cap before any delivery attempt at
	// all -- a different failure mode with a different counter).
	permanentlyFailed atomic.Int64
}

func NewNotifierQueue(d *Dispatcher, capacity int) *NotifierQueue {
	if capacity < 1 {
		capacity = 1
	}
	q := &NotifierQueue{cap: capacity, wake: make(chan struct{}, 1), now: time.Now}
	q.disp.Store(d)
	return q
}

func (q *NotifierQueue) SetDispatcher(d *Dispatcher) { q.disp.Store(d) }
func (q *NotifierQueue) Dropped() int64              { return q.dropped.Load() }

// PermanentlyFailed returns how many alerts have exhausted their retry
// budget (notifierMaxAttempts/notifierRetryWindow, or the undroppable
// variants) with at least one channel still failing.
func (q *NotifierQueue) PermanentlyFailed() int64 { return q.permanentlyFailed.Load() }

// setNowForTest overrides q's clock; tests use it to drive retry/exhaustion
// decisions deterministically without waiting on real time.
func (q *NotifierQueue) setNowForTest(now func() time.Time) { q.now = now }

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
	q.items = append(q.items, queuedAlert{a: a, quiet: quiet, firstEnqueued: q.now()})
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

// popDue returns the first item (in slice order) whose retryAt is due
// (<= now), removing it from the queue. If the queue has items but none are
// due yet, it reports ok=false along with wait, the duration until the
// earliest one becomes due, so Run knows how long it can sleep instead of
// busy-polling. At this queue's scale (bounded by cap, plus a handful of
// in-flight retries) a linear scan for the earliest retryAt is fine -- no
// heap needed.
func (q *NotifierQueue) popDue() (it queuedAlert, wait time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return queuedAlert{}, 0, false
	}
	now := q.now()
	for i, cand := range q.items {
		if !cand.retryAt.After(now) {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return cand, 0, true
		}
	}
	earliest := q.items[0].retryAt
	for _, cand := range q.items[1:] {
		if cand.retryAt.Before(earliest) {
			earliest = cand.retryAt
		}
	}
	wait = earliest.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return queuedAlert{}, wait, false
}

func (q *NotifierQueue) Run(ctx context.Context) {
	for {
		it, wait, ok := q.popDue()
		if !ok {
			if wait <= 0 {
				select {
				case <-ctx.Done():
					return
				case <-q.wake:
					continue
				}
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-q.wake:
				timer.Stop()
				continue
			case <-timer.C:
			}
			continue
		}
		q.attemptDelivery(it)
	}
}

// deliverOnce runs one delivery attempt for it: the full matched set
// (Dispatch) on a first attempt (it.targetChannels is nil/empty), or just
// the channels that failed last time (DispatchTo) on a retry. A nil
// dispatcher (SetDispatcher never called, or startup race) is a silent
// no-op, exactly as before this change.
func (q *NotifierQueue) deliverOnce(it queuedAlert) []DeliveryResult {
	d := q.disp.Load()
	if d == nil {
		return nil
	}
	if len(it.targetChannels) == 0 {
		return d.Dispatch(it.a, it.quiet)
	}
	return d.DispatchTo(it.a, it.quiet, it.targetChannels)
}

// nextRetry inspects results (from deliverOnce(it)) and decides whether to
// retry: nil/no error results in no retry (retry=false, nothing to do --
// either every channel succeeded, or there was no dispatcher to try).
// Otherwise, if it has budget left (notifierMaxAttempts/notifierRetryWindow,
// or the undroppable variants), it returns the updated item -- attempt
// incremented, targetChannels narrowed to just the channels that failed
// (never one that already succeeded, so no channel is ever double-delivered
// across the whole retry sequence), retryAt set via notifierBackoff -- for
// the caller to requeue. If the budget is exhausted, it logs and counts
// permanentlyFailed instead.
//
// A separate method (not inlined into Run) so a test can drive the
// retry/exhaustion decision directly against q's injected clock, without
// waiting on Run's own timers.
func (q *NotifierQueue) nextRetry(it queuedAlert, results []DeliveryResult) (next queuedAlert, retry bool) {
	var failed []string
	for _, r := range results {
		if r.Err != nil {
			failed = append(failed, r.Channel)
		}
	}
	if len(failed) == 0 {
		return queuedAlert{}, false
	}
	maxAttempts, window := notifierMaxAttempts, notifierRetryWindow
	if undroppable(it.a) {
		maxAttempts, window = notifierUndroppableMaxAttempts, notifierUndroppableRetryWindow
	}
	now := q.now()
	attempt := it.attempt + 1
	if attempt >= maxAttempts || now.Sub(it.firstEnqueued) >= window {
		q.permanentlyFailed.Add(1)
		log.Printf("notifier: alert %s permanently undelivered to %v after %d attempts over %s",
			it.a.Key, failed, attempt, now.Sub(it.firstEnqueued).Round(time.Second))
		return queuedAlert{}, false
	}
	it.attempt = attempt
	it.targetChannels = failed
	it.retryAt = now.Add(notifierBackoff(attempt))
	return it, true
}

// attemptDelivery runs one delivery attempt for it and, if nextRetry says
// to, requeues the updated item and wakes Run's loop so it can recompute
// how long to wait for the new retryAt.
func (q *NotifierQueue) attemptDelivery(it queuedAlert) {
	results := q.deliverOnce(it)
	next, retry := q.nextRetry(it, results)
	if !retry {
		return
	}
	q.mu.Lock()
	q.items = append(q.items, next)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
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
