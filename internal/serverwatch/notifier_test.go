package serverwatch

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeNotifier records every Alert it receives and can be configured to
// error, block until ctx is done, or panic.
type fakeNotifier struct {
	name   string
	mu     sync.Mutex
	got    []Alert
	err    error
	block  time.Duration
	panics bool
}

func (f *fakeNotifier) Name() string { return f.name }

func (f *fakeNotifier) Send(ctx context.Context, a Alert) error {
	if f.panics {
		panic("boom: " + f.name)
	}
	if f.block > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.block):
			// fall through to record below
		}
	}
	f.mu.Lock()
	f.got = append(f.got, a)
	f.mu.Unlock()
	return f.err
}

func (f *fakeNotifier) received() []Alert {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Alert, len(f.got))
	copy(out, f.got)
	return out
}

func TestDispatcherFansOutToAll(t *testing.T) {
	n1 := &fakeNotifier{name: "n1"}
	n2 := &fakeNotifier{name: "n2"}
	n3 := &fakeNotifier{name: "n3"}
	d := NewDispatcher([]Notifier{n1, n2, n3}, time.Second)

	a := Alert{Key: "cpu", Title: "CPU high", Body: "cpu at 99%", Severity: SevWarning, Kind: "fire", Source: "host1", Time: 1234}
	results := d.Dispatch(a)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("channel %s: unexpected error: %v", r.Channel, r.Err)
		}
	}
	for _, n := range []*fakeNotifier{n1, n2, n3} {
		got := n.received()
		if len(got) != 1 || got[0] != a {
			t.Errorf("%s: expected to receive %+v, got %+v", n.name, a, got)
		}
	}
}

func TestDispatcherIsolatesFailures(t *testing.T) {
	failing := &fakeNotifier{name: "failing", err: errors.New("send failed")}
	ok1 := &fakeNotifier{name: "ok1"}
	ok2 := &fakeNotifier{name: "ok2"}
	d := NewDispatcher([]Notifier{failing, ok1, ok2}, time.Second)

	a := Alert{Key: "disk", Kind: "fire", Severity: SevCritical}
	results := d.Dispatch(a)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	byChannel := map[string]error{}
	for _, r := range results {
		byChannel[r.Channel] = r.Err
	}
	if byChannel["failing"] == nil {
		t.Error("expected error result for 'failing' notifier")
	}
	if byChannel["ok1"] != nil {
		t.Errorf("ok1: unexpected error: %v", byChannel["ok1"])
	}
	if byChannel["ok2"] != nil {
		t.Errorf("ok2: unexpected error: %v", byChannel["ok2"])
	}
	if len(ok1.received()) != 1 || len(ok2.received()) != 1 {
		t.Error("expected ok1 and ok2 to still receive the alert")
	}
}

func TestDispatcherTimeout(t *testing.T) {
	slow := &fakeNotifier{name: "slow", block: 2 * time.Second}
	fast := &fakeNotifier{name: "fast"}
	d := NewDispatcher([]Notifier{slow, fast}, 50*time.Millisecond)

	start := time.Now()
	results := d.Dispatch(Alert{Key: "mem", Kind: "fire"})
	elapsed := time.Since(start)

	if elapsed >= 200*time.Millisecond {
		t.Fatalf("Dispatch took %v, expected to return promptly (well under the 2s block)", elapsed)
	}

	byChannel := map[string]error{}
	for _, r := range results {
		byChannel[r.Channel] = r.Err
	}
	if byChannel["slow"] == nil {
		t.Error("expected a timeout error for 'slow' notifier")
	}
	if byChannel["fast"] != nil {
		t.Errorf("fast: unexpected error: %v", byChannel["fast"])
	}
}

func TestDispatcherRecoversPanic(t *testing.T) {
	panicky := &fakeNotifier{name: "panicky", panics: true}
	ok := &fakeNotifier{name: "ok"}
	d := NewDispatcher([]Notifier{panicky, ok}, time.Second)

	results := d.Dispatch(Alert{Key: "swap", Kind: "fire"})

	byChannel := map[string]error{}
	for _, r := range results {
		byChannel[r.Channel] = r.Err
	}
	if byChannel["panicky"] == nil {
		t.Error("expected recovered-panic error for 'panicky' notifier")
	}
	if byChannel["ok"] != nil {
		t.Errorf("ok: unexpected error: %v", byChannel["ok"])
	}
	if len(ok.received()) != 1 {
		t.Error("expected 'ok' notifier to still receive the alert")
	}
}

func TestDispatcherTimeoutsRunConcurrentlyNotSerially(t *testing.T) {
	// Regression guard: if Dispatch ever waited out each notifier's timeout
	// one at a time instead of racing them all in parallel, N slow
	// notifiers would take N*timeout instead of ~timeout.
	const n = 8
	notifiers := make([]Notifier, n)
	for i := range notifiers {
		notifiers[i] = &fakeNotifier{name: fmt.Sprintf("slow%d", i), block: time.Second}
	}
	d := NewDispatcher(notifiers, 50*time.Millisecond)

	start := time.Now()
	results := d.Dispatch(Alert{Key: "many"})
	elapsed := time.Since(start)

	if elapsed >= 300*time.Millisecond {
		t.Fatalf("Dispatch took %v with %d slow notifiers; expected ~timeout, not timeout*%d", elapsed, n, n)
	}
	for _, r := range results {
		if r.Err == nil {
			t.Errorf("channel %s: expected timeout error", r.Channel)
		}
	}
}

func TestSeverityParseString(t *testing.T) {
	cases := []struct {
		sev Severity
		str string
	}{
		{SevInfo, "info"},
		{SevWarning, "warning"},
		{SevCritical, "critical"},
	}
	for _, c := range cases {
		if got := c.sev.String(); got != c.str {
			t.Errorf("Severity(%d).String() = %q, want %q", c.sev, got, c.str)
		}
		parsed, err := ParseSeverity(c.str)
		if err != nil {
			t.Errorf("ParseSeverity(%q) error: %v", c.str, err)
		}
		if parsed != c.sev {
			t.Errorf("ParseSeverity(%q) = %v, want %v", c.str, parsed, c.sev)
		}
	}

	if _, err := ParseSeverity("junk"); err == nil {
		t.Error("expected error for junk severity string")
	}
}
