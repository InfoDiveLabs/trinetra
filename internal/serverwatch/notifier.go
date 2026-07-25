package serverwatch

import (
	"context"
	"fmt"
	"sync"
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

// Dispatcher fans an Alert out to a set of Notifiers concurrently, bounding
// each delivery attempt with a timeout so one slow or broken channel can't
// hold up the others.
type Dispatcher struct {
	notifiers []Notifier
	timeout   time.Duration
}

// NewDispatcher builds a Dispatcher that sends to notifiers, giving each
// Send call up to timeout to complete.
func NewDispatcher(notifiers []Notifier, timeout time.Duration) *Dispatcher {
	return &Dispatcher{notifiers: notifiers, timeout: timeout}
}

// Dispatch sends a to every notifier concurrently, returning one
// DeliveryResult per notifier. It never panics: a Notifier.Send that panics
// is recovered and reported as an error.
//
// Dispatch itself returns within roughly d.timeout regardless of whether a
// given Notifier.Send honors its context: each send runs in its own
// goroutine and Dispatch races that goroutine's result against a local
// timer rather than blocking on it, so a Send that ignores ctx and hangs
// forever cannot delay Dispatch's return (though its goroutine will leak
// until the misbehaving call eventually completes — cooperative
// cancellation via ctx remains the well-behaved path).
func (d *Dispatcher) Dispatch(a Alert) []DeliveryResult {
	results := make([]DeliveryResult, len(d.notifiers))

	var wg sync.WaitGroup
	for i, n := range d.notifiers {
		wg.Add(1)
		go func(i int, n Notifier) {
			defer wg.Done()
			results[i] = DeliveryResult{Channel: n.Name(), Err: d.collect(n, a)}
		}(i, n)
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
