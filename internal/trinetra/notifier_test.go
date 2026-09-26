package trinetra

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

// allowAllChannel wraps a fake notifier in a Channel whose zero-value Route
// (MinSeverity=SevInfo, no kind filters) allows everything when not quiet.
func allowAllChannel(n Notifier) Channel {
	return Channel{N: n, Route: Route{}, Enabled: true}
}

func TestDispatcherFansOutToAll(t *testing.T) {
	n1 := &fakeNotifier{name: "n1"}
	n2 := &fakeNotifier{name: "n2"}
	n3 := &fakeNotifier{name: "n3"}
	d := NewDispatcher([]Channel{allowAllChannel(n1), allowAllChannel(n2), allowAllChannel(n3)}, time.Second)

	a := Alert{Key: "cpu", Title: "CPU high", Body: "cpu at 99%", Severity: SevWarning, Kind: "fire", Source: "host1", Time: 1234}
	results := d.Dispatch(a, false)

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

// TestDispatcherDispatchToNamedSubset covers fleet routing (task 5): only
// the named channel receives the alert, even though every channel is
// enabled and would otherwise allow it.
func TestDispatcherDispatchToNamedSubset(t *testing.T) {
	slack := &fakeNotifier{name: "slack"}
	pager := &fakeNotifier{name: "pager"}
	d := NewDispatcher([]Channel{allowAllChannel(slack), allowAllChannel(pager)}, time.Second)

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire"}
	results := d.DispatchTo(a, false, []string{"slack"})
	if len(results) != 1 || results[0].Channel != "slack" {
		t.Fatalf("results = %+v, want exactly slack", results)
	}
	if got := slack.received(); len(got) != 1 {
		t.Fatalf("slack received %d, want 1", len(got))
	}
	if got := pager.received(); len(got) != 0 {
		t.Fatalf("pager received %d, want 0 (not named)", len(got))
	}
}

// TestDispatcherDispatchToWildcardActsLikeDispatch: "*" reaches every
// enabled, routing-matched channel, exactly like Dispatch.
func TestDispatcherDispatchToWildcardActsLikeDispatch(t *testing.T) {
	n1 := &fakeNotifier{name: "n1"}
	n2 := &fakeNotifier{name: "n2"}
	d := NewDispatcher([]Channel{allowAllChannel(n1), allowAllChannel(n2)}, time.Second)

	results := d.DispatchTo(Alert{Key: "cpu", Severity: SevWarning, Kind: "fire"}, false, []string{"*"})
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (every enabled channel)", len(results))
	}
}

// TestDispatcherDispatchToStillGatesOnRouteAllows covers the task-5 ruling:
// naming a channel never bypasses its own Route.Allows -- quiet hours and
// severity gating still apply exactly as they do for Dispatch.
func TestDispatcherDispatchToStillGatesOnRouteAllows(t *testing.T) {
	slack := &fakeNotifier{name: "slack"}
	pager := &fakeNotifier{name: "pager"}
	d := NewDispatcher([]Channel{
		{N: slack, Route: Route{MinSeverity: SevWarning}, Enabled: true},
		{N: pager, Route: Route{MinSeverity: SevWarning, CriticalOverridesQuiet: true}, Enabled: true},
	}, time.Second)

	// During quiet hours, a non-critical alert is gated out on EVERY channel,
	// named or not.
	results := d.DispatchTo(Alert{Key: "cpu", Severity: SevWarning, Kind: "fire"}, true, []string{"slack", "pager"})
	if len(results) != 0 {
		t.Fatalf("results during quiet hours = %+v, want none (severity below critical)", results)
	}

	// A critical alert during quiet hours still only reaches the channel
	// whose Route explicitly overrides quiet hours.
	results = d.DispatchTo(Alert{Key: "cpu", Severity: SevCritical, Kind: "fire"}, true, []string{"slack", "pager"})
	if len(results) != 1 || results[0].Channel != "pager" {
		t.Fatalf("results for a critical alert during quiet hours = %+v, want only pager", results)
	}

	// Naming a channel not in the config's channel set (config drift) simply
	// delivers to nothing -- no panic, no error.
	results = d.DispatchTo(Alert{Key: "cpu", Severity: SevCritical, Kind: "fire"}, false, []string{"does-not-exist"})
	if len(results) != 0 {
		t.Fatalf("results for an unknown channel name = %+v, want none", results)
	}
}

func TestDispatcherIsolatesFailures(t *testing.T) {
	failing := &fakeNotifier{name: "failing", err: errors.New("send failed")}
	ok1 := &fakeNotifier{name: "ok1"}
	ok2 := &fakeNotifier{name: "ok2"}
	d := NewDispatcher([]Channel{allowAllChannel(failing), allowAllChannel(ok1), allowAllChannel(ok2)}, time.Second)

	a := Alert{Key: "disk", Kind: "fire", Severity: SevCritical}
	results := d.Dispatch(a, false)

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
	d := NewDispatcher([]Channel{allowAllChannel(slow), allowAllChannel(fast)}, 50*time.Millisecond)

	start := time.Now()
	results := d.Dispatch(Alert{Key: "mem", Kind: "fire"}, false)
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
	d := NewDispatcher([]Channel{allowAllChannel(panicky), allowAllChannel(ok)}, time.Second)

	results := d.Dispatch(Alert{Key: "swap", Kind: "fire"}, false)

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
	channels := make([]Channel, n)
	for i := range channels {
		channels[i] = allowAllChannel(&fakeNotifier{name: fmt.Sprintf("slow%d", i), block: time.Second})
	}
	d := NewDispatcher(channels, 50*time.Millisecond)

	start := time.Now()
	results := d.Dispatch(Alert{Key: "many"}, false)
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

type recordingNotifier struct {
	name string
	sent *[]string
	mu   *sync.Mutex
}

func (r recordingNotifier) Name() string { return r.name }
func (r recordingNotifier) Send(ctx context.Context, a Alert) error {
	r.mu.Lock()
	*r.sent = append(*r.sent, a.Key)
	r.mu.Unlock()
	return nil
}

func TestNotifierQueueDropsOldestNonCriticalKeepsCritical(t *testing.T) {
	q := NewNotifierQueue(nil, 2) // nil dispatcher: not started, just testing the queue policy

	q.Enqueue(Alert{Key: "info1", Severity: SevInfo, Kind: "fire"}, false)
	q.Enqueue(Alert{Key: "info2", Severity: SevInfo, Kind: "fire"}, false)
	// queue full (cap 2); a critical must evict the oldest non-critical.
	q.Enqueue(Alert{Key: "crit", Severity: SevCritical, Kind: "fire"}, false)

	keys := q.snapshotKeysForTest()
	if !containsString(keys, "crit") {
		t.Fatalf("critical alert was dropped; queue=%v", keys)
	}
	if containsString(keys, "info1") {
		t.Fatalf("oldest non-critical not evicted; queue=%v", keys)
	}
	if q.Dropped() != 1 {
		t.Fatalf("Dropped()=%d, want 1", q.Dropped())
	}
}

func TestNotifierQueueAllUndroppableDropsDroppableNewcomer(t *testing.T) {
	q := NewNotifierQueue(nil, 2)

	q.Enqueue(Alert{Key: "crit1", Severity: SevCritical, Kind: "fire"}, false)
	q.Enqueue(Alert{Key: "recover1", Severity: SevInfo, Kind: "recover"}, false)
	// queue full (cap 2) and every item is undroppable; a droppable newcomer
	// must be dropped itself rather than evicting a reserved alert.
	q.Enqueue(Alert{Key: "info1", Severity: SevInfo, Kind: "fire"}, false)

	keys := q.snapshotKeysForTest()
	if len(keys) != 2 {
		t.Fatalf("queue length=%d, want 2 (unchanged); queue=%v", len(keys), keys)
	}
	if containsString(keys, "info1") {
		t.Fatalf("droppable newcomer was kept instead of dropped; queue=%v", keys)
	}
	if !containsString(keys, "crit1") || !containsString(keys, "recover1") {
		t.Fatalf("a reserved (undroppable) alert was evicted; queue=%v", keys)
	}
	if q.Dropped() != 1 {
		t.Fatalf("Dropped()=%d, want 1", q.Dropped())
	}
}

func TestNotifierQueueAllUndroppableGrowsPastCapForUndroppableNewcomer(t *testing.T) {
	q := NewNotifierQueue(nil, 2)

	q.Enqueue(Alert{Key: "crit1", Severity: SevCritical, Kind: "fire"}, false)
	q.Enqueue(Alert{Key: "recover1", Severity: SevInfo, Kind: "recover"}, false)
	// queue full (cap 2) and every item is undroppable; an undroppable
	// newcomer must be admitted even though that grows the queue past cap,
	// since nothing droppable exists to make room and nothing may be dropped.
	q.Enqueue(Alert{Key: "crit2", Severity: SevCritical, Kind: "fire"}, false)

	keys := q.snapshotKeysForTest()
	if len(keys) != 3 {
		t.Fatalf("queue length=%d, want 3 (grown past cap); queue=%v", len(keys), keys)
	}
	if !containsString(keys, "crit1") || !containsString(keys, "recover1") || !containsString(keys, "crit2") {
		t.Fatalf("expected all three undroppable alerts retained; queue=%v", keys)
	}
	if q.Dropped() != 0 {
		t.Fatalf("Dropped()=%d, want 0 (nothing should be dropped)", q.Dropped())
	}
}

func TestNotifierQueueRunDelivers(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	disp := NewDispatcher([]Channel{{
		N: recordingNotifier{name: "rec", sent: &sent, mu: &mu}, Enabled: true,
		Route: Route{MinSeverity: SevInfo},
	}}, dispatcherTimeout)
	q := NewNotifierQueue(disp, 8)

	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	q.Enqueue(Alert{Key: "a", Severity: SevInfo, Kind: "fire"}, false)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n == 1 {
			cancel()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("alert was not delivered")
}

// TestNotifierQueueSetDispatcherReroutesDelivery guards the mechanism the
// daemon's dispatcher rebuilds (applyConfig AND setChatID) depend on: because
// delivery runs off the queue's OWN dispatcher pointer, a rebuild must call
// q.SetDispatcher or alerts keep going to the stale dispatcher. This was the
// setChatID bug -- a chat id auto-captured via /start enrollment rebuilt the
// shared dispatcher but never told the queue, so every alert after a fresh
// zero-config enrollment dispatched to the stale channel-less dispatcher and
// silently didn't deliver. Here the "old" dispatcher stands in for that stale
// pre-enrollment one and the "new" one for the freshly-enrolled channel: after
// SetDispatcher, an enqueued alert must reach the NEW notifier and never the
// old. If SetDispatcher didn't actually swap the active dispatcher, the alert
// would land on oldN and this test fails.
func TestNotifierQueueSetDispatcherReroutesDelivery(t *testing.T) {
	oldN := &fakeNotifier{name: "stale-pre-enrollment"}
	newN := &fakeNotifier{name: "freshly-enrolled"}

	q := NewNotifierQueue(NewDispatcher([]Channel{allowAllChannel(oldN)}, dispatcherTimeout), 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	// The daemon's setChatID/applyConfig rebuild the dispatcher then propagate it
	// to the queue via SetDispatcher; model exactly that swap.
	q.SetDispatcher(NewDispatcher([]Channel{allowAllChannel(newN)}, dispatcherTimeout))

	q.Enqueue(Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1}, false)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(newN.received()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := len(newN.received()); got != 1 {
		t.Fatalf("newly-configured dispatcher received %d alerts, want 1: dispatcher swap did not reach the queue", got)
	}
	if got := len(oldN.received()); got != 0 {
		t.Fatalf("stale dispatcher received %d alerts, want 0: delivery still using the pre-swap dispatcher", got)
	}
}
