package trinetra

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// namedDelivery records one channel-scoped delivery attempt a routingFixture observed.
type namedDelivery struct {
	a        Alert
	channels []string
}

// routingFixture wires a fleetAlertEngine with routing/escalation active (SetRouting),
// backed by a real incidentStore and alertingStore (on disk, like production).
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

// newRoutingFixtureAt builds a routingFixture rooted at dir (an explicit, caller-owned
// directory rather than a fresh t.TempDir()), starting its clock at now.
func newRoutingFixtureAt(t *testing.T, dir string, cfg core.AlertingConfig, now time.Time) *routingFixture {
	t.Helper()
	disableGroupWaitForTest(t)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	rf := &routingFixture{incidents: incidents, now: now}
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

func newRoutingFixture(t *testing.T, cfg core.AlertingConfig) *routingFixture {
	return newRoutingFixtureAt(t, t.TempDir(), cfg, time.Unix(1_700_000_000, 0))
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

func (rf *routingFixture) allDispatched() []namedDelivery {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return append([]namedDelivery(nil), rf.dispatchedTo...)
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

// TestEngineRoutingDeliversFireToResolvedChannels: with a routing config wired, a fire is
// delivered through deliverNamed to step 0's resolved channels, not e.deliver.
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
	if last.Kind != "delivered" || last.Detail != "fire: policy p step 0: slack" {
		t.Fatalf("last event = %+v, want a policy-labelled step-0 delivered event", last)
	}
}

// TestEngineEscalationFiresAfterDelayAndIsIdempotent: a step whose After has elapsed, on a
// still-firing/unacked incident, is escalated exactly once even across repeated ticks.
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
		if ev.Kind == "escalated" && ev.Detail == "policy esc step 1: pager" {
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

// TestEngineEscalationStopsOnAck: an acked incident is no longer state "firing", so
// TickEscalations must never escalate it even once its step is due.
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

// TestEngineEscalationStopsOnResolve: a resolved incident is no longer "firing" either, so
// a recover before the step is due permanently prevents its escalation.
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

// TestEngineRepeatEveryRenotifiesLastReachedStep: once the last step is reached,
// RepeatEvery re-notifies its channels on that cadence while firing and unacked.
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

// TestEngineResolvedGoesToUnionOfDeliveredChannels: the resolved message goes
// to every channel that received any step of the fire leg, not just step 0's.
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

// TestEngineSendResolvedFalseSuppressesRecoverDelivery: a policy with SendResolved false
// never delivers the recover, even though it is still recorded.
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

// twoIndependentPolicies builds a Continue-chained route pair matching every alert, each
// referencing its own policy.
func twoIndependentPolicies(a, b core.Policy) core.AlertingConfig {
	return core.AlertingConfig{
		Routes: []core.Route{
			{Name: "rA", Matchers: []core.Matcher{{Rule: "*"}}, Policy: a.Name, Continue: true},
			{Name: "rB", Matchers: []core.Matcher{{Rule: "*"}}, Policy: b.Name},
		},
		Policies:      []core.Policy{a, b},
		DefaultPolicy: a.Name,
	}
}

// TestEngineEscalationEachMatchedPolicyIndependent: policy A's step 1 (After 5m, chanX) and
// policy B's step 1 (After 30m, chanY) must fire on THEIR OWN schedules.
func TestEngineEscalationEachMatchedPolicyIndependent(t *testing.T) {
	policyA := core.Policy{Name: "A", Steps: []core.PolicyStep{
		{After: "0s", Channels: []string{"base"}}, {After: "5m", Channels: []string{"chanX"}},
	}}
	policyB := core.Policy{Name: "B", Steps: []core.PolicyStep{
		{After: "0s", Channels: []string{"base"}}, {After: "30m", Channels: []string{"chanY"}},
	}}
	rf := newRoutingFixture(t, twoIndependentPolicies(policyA, policyB))
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(6 * time.Minute) // past A's 5m, nowhere near B's 30m
	rf.tick()
	got := rf.allDispatched()
	if len(got) != 1 || len(got[0].channels) != 1 || got[0].channels[0] != "chanX" {
		t.Fatalf("dispatched at 6m = %+v, want exactly chanX (A's step 1) -- chanY must NOT be paged early", got)
	}

	rf.advance(10 * time.Minute) // 16m total: still nowhere near B's 30m
	rf.tick()
	if got := rf.allDispatched(); len(got) != 1 {
		t.Fatalf("dispatched at 16m = %+v, want still just chanX (B's step is not due until 30m)", got)
	}

	rf.advance(15 * time.Minute) // 31m total: B's step 1 is now due
	rf.tick()
	got = rf.allDispatched()
	if len(got) != 2 || got[1].channels[0] != "chanY" {
		t.Fatalf("dispatched at 31m = %+v, want chanX then chanY", got)
	}
}

// TestEngineRepeatEveryPerPolicyCadence: each matched policy repeats on its
// OWN RepeatEvery, independently.
func TestEngineRepeatEveryPerPolicyCadence(t *testing.T) {
	policyA := core.Policy{Name: "A", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"chanA"}}}, RepeatEvery: "10m"}
	policyB := core.Policy{Name: "B", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"chanB"}}}, RepeatEvery: "30m"}
	rf := newRoutingFixture(t, twoIndependentPolicies(policyA, policyB))
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(10*time.Minute + time.Second)
	rf.tick()
	got := rf.allDispatched()
	if len(got) != 1 || got[0].channels[0] != "chanA" {
		t.Fatalf("dispatched at 10m1s = %+v, want exactly one chanA repeat (B's 30m has not elapsed)", got)
	}

	rf.advance(20 * time.Minute) // 30m2s total: A's second 10m boundary AND B's first 30m boundary
	rf.tick()
	got = rf.allDispatched()
	if len(got) != 3 {
		t.Fatalf("dispatched at 30m2s = %+v, want 3 (A's 2nd repeat + B's 1st repeat)", got)
	}
	if got[1].channels[0] != "chanA" || got[2].channels[0] != "chanB" {
		t.Fatalf("dispatched[1:] = %+v, want [chanA chanB] (A processed before B, per route order)", got[1:])
	}
}

// TestEngineSendResolvedMixOnlySendsToTruePolicies: with one matched policy
// SendResolved=true and another false.
func TestEngineSendResolvedMixOnlySendsToTruePolicies(t *testing.T) {
	yes, no := true, false
	policyA := core.Policy{Name: "A", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"chanA"}}}, SendResolved: &yes}
	policyB := core.Policy{Name: "B", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"chanB"}}}, SendResolved: &no}
	rf := newRoutingFixture(t, twoIndependentPolicies(policyA, policyB))
	src := alertSource{}
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(time.Minute)
	rf.engine.Submit(src, Alert{Key: "cpu", Kind: "recover", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	if rf.deliveredCount() != 2 { // fire + recover
		t.Fatalf("deliveredTo = %d, want 2", rf.deliveredCount())
	}
	got := rf.lastDelivered().channels
	if len(got) != 1 || got[0] != "chanA" {
		t.Fatalf("resolved channels = %v, want exactly [chanA] (B opted out of SendResolved)", got)
	}
}

// TestEngineEscalationSurvivesRestartNoResend: an already-escalated step is never re-sent
// by a FRESH engine over the same incidentStore file.
func TestEngineEscalationSurvivesRestartNoResend(t *testing.T) {
	dir := t.TempDir()
	cfg := twoStepPolicy("5m", "10m") // single policy "esc": step 1 pager @5m, repeat every 10m
	start := time.Unix(1_700_000_000, 0)

	rf1 := newRoutingFixtureAt(t, dir, cfg, start)
	rf1.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: start.Unix()})
	rf1.waitIdle()
	rf1.advance(6 * time.Minute) // past step 1's 5m
	rf1.tick()
	if rf1.dispatchedCount() != 1 {
		t.Fatalf("pre-restart dispatchedTo = %d, want 1 (step 1 escalated)", rf1.dispatchedCount())
	}

	// "Restart": a brand-new incidentStore + engine instance over the SAME
	// on-disk files, at the same point in time escalateStep just recorded.
	rf2 := newRoutingFixtureAt(t, dir, cfg, rf1.now)
	rf2.tick()
	if rf2.dispatchedCount() != 0 {
		t.Fatalf("a fresh engine's own tick dispatched %d, want 0 (nothing new is due yet)", rf2.dispatchedCount())
	}

	// Still well past step 1's own 5m due time, but short of the 10m repeat_every boundary
	// (reached at +6m -> due again at +16m): confirms step 1 itself is not re-escalated.
	rf2.advance(9 * time.Minute) // start+15m: past step 1's due time, before the +16m repeat
	rf2.tick()
	if rf2.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo after restart + ticking past step 1's time (but before the repeat) = %d, want 0 (must not re-send)", rf2.dispatchedCount())
	}

	// RepeatEvery continues from the ORIGINAL escalated-at-6m timestamp
	// (nothing was ever repeated before the restart): due at start+16m.
	rf2.now = start.Add(16*time.Minute + time.Second)
	rf2.tick()
	if rf2.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after crossing the repeat interval post-restart = %d, want 1", rf2.dispatchedCount())
	}

	// A SECOND restart must continue from the "repeated" event just
	// recorded, not re-derive from the original "escalated" timestamp.
	rf3 := newRoutingFixtureAt(t, dir, cfg, rf2.now)
	rf3.now = start.Add(16*time.Minute + time.Second + 5*time.Minute) // before the NEXT repeat (10m later) is due
	rf3.tick()
	if rf3.dispatchedCount() != 0 {
		t.Fatalf("dispatchedTo too early for the next repeat post-restart = %d, want 0", rf3.dispatchedCount())
	}
	rf3.now = start.Add(16*time.Minute + time.Second + 10*time.Minute + time.Second)
	rf3.tick()
	if rf3.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after crossing the next repeat interval = %d, want 1", rf3.dispatchedCount())
	}
}

// --- structured timeline events ----------------------------

// TestEngineStructuredEventsSurviveAmbiguousChannelName: a channel literally NAMED "step 5:
// pager" could be misread by the text parser as a second step marker in Detail.
func TestEngineStructuredEventsSurviveAmbiguousChannelName(t *testing.T) {
	trickyChannel := "step 5: pager"
	cfg := twoStepPolicy("5m", "")
	cfg.Policies[0].Steps[1].Channels = []string{trickyChannel}
	rf := newRoutingFixture(t, cfg)
	rf.engine.Submit(alertSource{}, Alert{Key: "cpu", Kind: "fire", Severity: SevCritical, Time: rf.now.Unix()})
	rf.waitIdle()

	rf.advance(6 * time.Minute)
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo = %d, want 1", rf.dispatchedCount())
	}
	if got := rf.lastDispatched().channels; len(got) != 1 || got[0] != trickyChannel {
		t.Fatalf("escalated channels = %v, want [%q]", got, trickyChannel)
	}

	inc := rf.onlyIncident(t)
	var escalated core.IncidentEvent
	found := false
	for _, ev := range inc.Timeline {
		if ev.Kind == "escalated" {
			escalated, found = ev, true
		}
	}
	if !found {
		t.Fatalf("timeline missing the escalated event: %+v", inc.Timeline)
	}
	policy, step, channels, ok := stepEventInfo(escalated)
	if !ok || policy != "esc" || step != 1 || len(channels) != 1 || channels[0] != trickyChannel {
		t.Fatalf("stepEventInfo(escalated) = policy=%q step=%d channels=%v ok=%v, want policy=esc step=1 channels=[%q]",
			policy, step, channels, ok, trickyChannel)
	}

	// A later tick must NOT re-escalate: escalatedTo matches on the structured Policy/Step
	// fields, never on the (now ambiguous-looking) Detail text.
	rf.advance(time.Hour)
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after a further tick = %d, want still 1 (no re-escalation)", rf.dispatchedCount())
	}
}

// TestEngineLegacyTextEventsStillParse: an incident recorded by a build from before
// structured fields existed.
func TestEngineLegacyTextEventsStillParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	incidents, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{}
	inc, err := incidents.Apply(incidentApply{
		src: src, alert: Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A hand-built, PURELY LEGACY event: no Leg/AlertKey/Node/FiredAt/Policy
	// at all, exactly what an older build would have written.
	if _, err := incidents.AppendEvent(inc.ID, core.IncidentEvent{
		TS: 1010, Kind: "delivered", Detail: "fire: policy esc step 0: slack", Actor: "system",
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := incidents.Get(inc.ID)
	if !ok {
		t.Fatal("incident missing")
	}
	if fireDelivered, recoverDelivered := legDeliveredStatusFor(got, "", "fleet:node:x:down", 1000); !fireDelivered || recoverDelivered {
		t.Fatalf("legDeliveredStatusFor(legacy event) = fire=%v recover=%v, want fire=true recover=false", fireDelivered, recoverDelivered)
	}
	policy, step, channels, ok := stepEventInfo(got.Timeline[len(got.Timeline)-1])
	if !ok || policy != "esc" || step != 0 || len(channels) != 1 || channels[0] != "slack" {
		t.Fatalf("stepEventInfo(legacy event) = policy=%q step=%d channels=%v ok=%v", policy, step, channels, ok)
	}
}
