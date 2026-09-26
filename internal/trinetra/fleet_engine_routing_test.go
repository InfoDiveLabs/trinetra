package trinetra

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// namedDelivery records one channel-scoped delivery attempt a routingFixture
// observed (either a real fire/recover leg, via deliverNamed, or an
// escalation/repeat notification, via dispatchOnly).
type namedDelivery struct {
	a        Alert
	channels []string
}

// routingFixture wires a fleetAlertEngine with routing/escalation active
// (SetRouting), backed by a real incidentStore and alertingStore (on disk,
// like production), so a test can assert exactly which channels each
// delivery/escalation/repeat went to without any network or dispatcher
// machinery.
type routingFixture struct {
	mu           sync.Mutex
	deliveredTo  []namedDelivery // fire/recover legs (deliverNamed)
	dispatchedTo []namedDelivery // escalation/repeat (dispatchOnly)
	deliverFail  bool

	incidents *incidentStore
	engine    *fleetAlertEngine
	alerting  *alertingStore
	now       time.Time
}

func newRoutingFixture(t *testing.T, cfg core.AlertingConfig) *routingFixture {
	t.Helper()
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rf := &routingFixture{incidents: incidents, now: time.Unix(1_700_000_000, 0)}
	rf.engine = newFleetAlertEngine(
		func() time.Time { return rf.now },
		func(Alert) bool { return true }, // unused once routing is wired
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	alerting, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Policies) > 0 || len(cfg.Routes) > 0 || cfg.DefaultPolicy != "" {
		if _, err := alerting.Set(cfg, allChannelsValid); err != nil {
			t.Fatal(err)
		}
	}
	rf.alerting = alerting
	rf.engine.SetRouting(alerting,
		func(a Alert, channels []string) bool {
			rf.mu.Lock()
			defer rf.mu.Unlock()
			if rf.deliverFail {
				return false
			}
			rf.deliveredTo = append(rf.deliveredTo, namedDelivery{a, channels})
			return true
		},
		func(a Alert, channels []string) bool {
			rf.mu.Lock()
			defer rf.mu.Unlock()
			rf.dispatchedTo = append(rf.dispatchedTo, namedDelivery{a, channels})
			return true
		},
	)
	return rf
}

func (rf *routingFixture) waitIdle() { rf.engine.waitIdleForTest() }

func (rf *routingFixture) tick() {
	rf.engine.TickEscalations(rf.now)
	rf.waitIdle()
}

func (rf *routingFixture) advance(d time.Duration) {
	rf.now = rf.now.Add(d)
}

func (rf *routingFixture) deliveredCount() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return len(rf.deliveredTo)
}

func (rf *routingFixture) dispatchedCount() int {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return len(rf.dispatchedTo)
}

func (rf *routingFixture) lastDispatched() namedDelivery {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.dispatchedTo[len(rf.dispatchedTo)-1]
}

func (rf *routingFixture) lastDelivered() namedDelivery {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return rf.deliveredTo[len(rf.deliveredTo)-1]
}

func (rf *routingFixture) onlyIncident(t *testing.T) core.Incident {
	t.Helper()
	incs := rf.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1: %+v", len(incs), incs)
	}
	return incs[0]
}

func twoStepPolicy(after2, repeatEvery string) core.AlertingConfig {
	return core.AlertingConfig{
		Policies: []core.Policy{{
			Name: "esc",
			Steps: []core.PolicyStep{
				{After: "0s", Channels: []string{"slack"}},
				{After: after2, Channels: []string{"pager"}},
			},
			RepeatEvery: repeatEvery,
		}},
		DefaultPolicy: "esc",
	}
}

// TestEngineRoutingDeliversFireToResolvedChannels: with a routing config
// wired, a fire is delivered through deliverNamed to step 0's resolved
// channels, not e.deliver (the pre-routing path).
func TestEngineRoutingDeliversFireToResolvedChannels(t *testing.T) {
	rf := newRoutingFixture(t, core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}}},
		DefaultPolicy: "p",
	})
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevWarning, Time: rf.now.Unix()})
	rf.waitIdle()

	if rf.deliveredCount() != 1 {
		t.Fatalf("deliveredTo = %d, want 1", rf.deliveredCount())
	}
	if got := rf.lastDelivered().channels; len(got) != 1 || got[0] != "slack" {
		t.Fatalf("channels = %v, want [slack]", got)
	}
	inc := rf.onlyIncident(t)
	last := inc.Timeline[len(inc.Timeline)-1]
	if last.Kind != "delivered" || last.Detail != "fire: step 0: slack" {
		t.Fatalf("last event = %+v, want a step-0 delivered event", last)
	}
}

// TestEngineEscalationFiresAfterDelayAndIsIdempotent covers the task-5
// ruling: a step whose After has elapsed, on a still-firing/unacked
// incident, is escalated exactly once even across repeated ticks.
func TestEngineEscalationFiresAfterDelayAndIsIdempotent(t *testing.T) {
	rf := newRoutingFixture(t, twoStepPolicy("5m", ""))
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(4 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo = %d before the step is due, want 0", rf.dispatchedCount())
	}

	rf.advance(2 * time.Minute) // now 6m since fire: step 1 (After 5m) is due
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo = %d after the step is due, want 1", rf.dispatchedCount())
	}
	if got := rf.lastDispatched().channels; len(got) != 1 || got[0] != "pager" {
		t.Fatalf("escalated channels = %v, want [pager]", got)
	}
	inc := rf.onlyIncident(t)
	found := false
	for _, ev := range inc.Timeline {
		if ev.Kind == "escalated" && ev.Detail == "step 1: pager" {
			found = true
		}
	}
	if !found {
		t.Fatalf("timeline missing the escalated event: %+v", inc.Timeline)
	}

	// A second, third... tick after the step is already reached must NOT
	// escalate it again (durable, timeline-derived state).
	rf.tick()
	rf.advance(time.Hour)
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after further ticks = %d, want still 1 (no re-escalation)", rf.dispatchedCount())
	}
}

// TestEngineEscalationStopsOnAck: an acked incident is no longer state
// "firing", so TickEscalations must never escalate it even once its step is
// due.
func TestEngineEscalationStopsOnAck(t *testing.T) {
	rf := newRoutingFixture(t, twoStepPolicy("5m", ""))
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	inc := rf.onlyIncident(t)
	if _, err := rf.incidents.Ack(inc.ID, "cli", rf.now.Unix()); err != nil {
		t.Fatal(err)
	}

	rf.advance(10 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo = %d for an ACKED incident, want 0", rf.dispatchedCount())
	}
}

// TestEngineEscalationStopsOnResolve: a resolved incident is no longer
// "firing" either, so a recover before the step is due permanently prevents
// its escalation.
func TestEngineEscalationStopsOnResolve(t *testing.T) {
	rf := newRoutingFixture(t, twoStepPolicy("5m", ""))
	src := alertSource{}
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(time.Minute)
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "recover", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(10 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo = %d for a RESOLVED incident, want 0", rf.dispatchedCount())
	}
}

// TestEngineRepeatEveryRenotifiesLastReachedStep covers the task-5 ruling:
// once the last step is reached, RepeatEvery re-notifies its channels on
// that cadence while firing and unacked.
func TestEngineRepeatEveryRenotifiesLastReachedStep(t *testing.T) {
	rf := newRoutingFixture(t, core.AlertingConfig{
		Policies: []core.Policy{{
			Name:        "p",
			Steps:       []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}},
			RepeatEvery: "10m",
		}},
		DefaultPolicy: "p",
	})
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(9 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo before repeat_every elapsed = %d, want 0", rf.dispatchedCount())
	}

	rf.advance(2 * time.Minute) // 11m since fire
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after repeat_every elapsed = %d, want 1", rf.dispatchedCount())
	}
	if got := rf.lastDispatched().channels; len(got) != 1 || got[0] != "slack" {
		t.Fatalf("repeated channels = %v, want [slack]", got)
	}

	// No second repeat until ANOTHER full interval has passed.
	rf.advance(2 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo too early for a second repeat = %d, want still 1", rf.dispatchedCount())
	}
	rf.advance(9 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 2 {
		t.Fatalf("dispatchedTo after a second interval = %d, want 2", rf.dispatchedCount())
	}
}

// TestEngineResolvedGoesToUnionOfDeliveredChannels covers the task-5 ruling:
// the resolved message goes to every channel that received any step of the
// fire leg, not just step 0's own channels.
func TestEngineResolvedGoesToUnionOfDeliveredChannels(t *testing.T) {
	rf := newRoutingFixture(t, twoStepPolicy("1m", ""))
	src := alertSource{}
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(2 * time.Minute)
	rf.tick() // escalates step 1 (pager)
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo = %d, want the step-1 escalation to have fired", rf.dispatchedCount())
	}

	rf.advance(time.Minute)
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "recover", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	if rf.deliveredCount() != 2 { // fire (step 0) + recover
		t.Fatalf("deliveredTo = %d, want 2 (fire and recover)", rf.deliveredCount())
	}
	got := rf.lastDelivered().channels
	set := map[string]bool{}
	for _, c := range got {
		set[c] = true
	}
	if !set["slack"] || !set["pager"] {
		t.Fatalf("resolved channels = %v, want the union [slack pager]", got)
	}
}

// TestEngineSendResolvedFalseSuppressesRecoverDelivery: a policy with
// SendResolved false never delivers the recover, even though it is still
// recorded (existing incident-recording behaviour is unaffected by
// routing).
func TestEngineSendResolvedFalseSuppressesRecoverDelivery(t *testing.T) {
	no := false
	rf := newRoutingFixture(t, core.AlertingConfig{
		Policies: []core.Policy{{
			Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}, SendResolved: &no,
		}},
		DefaultPolicy: "p",
	})
	src := alertSource{}
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()
	rf.advance(time.Minute)
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "recover", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	if rf.deliveredCount() != 1 {
		t.Fatalf("deliveredTo = %d, want 1 (fire only; the recover must not be delivered)", rf.deliveredCount())
	}
	inc := rf.onlyIncident(t)
	if inc.State != "resolved" {
		t.Fatalf("incident state = %q, want resolved (still recorded)", inc.State)
	}
}
