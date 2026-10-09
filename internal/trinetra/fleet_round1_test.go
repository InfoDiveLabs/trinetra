// Package trinetra: fleet_round1_test.go covers per-member silence
// suppression, a dependency-folded member's recover finding the right bucket,
// grouped resurrection, a release sweep on SetNodeDeps and the
// group-interval/fallback-after interaction.
package trinetra

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// --- CRITICAL 1: per-member silence suppression ---------------------------

// TestEngineSilencedMemberFirstThenUnsilencedSiblingIsDelivered: a silenced
// member alone puts the incident in state "suppressed"; an unsilenced
// sibling joining afterward must still be delivered immediately (not
// starved because ITS sibling is silenced), and flips the incident back to
// "firing" (a per-member computation, not "whichever member fired last").
// Once the silence ends, the first member is delivered on its own, and the
// incident stays "firing" throughout.
func TestEngineSilencedMemberFirstThenUnsilencedSiblingIsDelivered(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", ef.now)
	ef.engine.PushLeaseNow("n2", ef.now)

	names := map[string]string{"n1": "box1", "n2": "box2"}
	store := newTestSilenceStore(t)
	if _, err := store.Create(core.Silence{Matchers: []core.Matcher{{Node: "box1"}}, Start: 0, End: ef.now.Unix() + 100, Author: "cli"}); err != nil {
		t.Fatal(err)
	}
	ef.engine.SetSilences(store, func(id string) (string, []string) { return names[id], nil })

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered after n1's (silenced) fire = %d, want 0", ef.deliveredCount())
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].State != "suppressed" {
		t.Fatalf("incident after n1 fires alone = %+v, want suppressed", incs)
	}
	id := incs[0].ID

	ef.now = ef.now.Add(5 * time.Second)
	ef.engine.HandleChildAlert("n2", "box2", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()

	inc, ok := ef.incidents.Get(id)
	if !ok || inc.State != "firing" {
		t.Fatalf("incident after n2 (unsilenced) joins = %+v, want firing (n1 alone must not starve n2)", inc)
	}
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered after n2's fire = %d, want 1 (n2 delivered, n1 still silenced)", ef.deliveredCount())
	}
	if title := ef.lastDelivered().Title; !strings.Contains(title, "box2") || strings.Contains(title, "box1") {
		t.Fatalf("delivered title = %q, want box2 only (box1 still silenced)", title)
	}

	// The silence ends (time passes its End): n1 is delivered alone, and the
	// incident stays firing.
	ef.now = ef.now.Add(200 * time.Second)
	ef.engine.TickSilences(ef.now, []string{"n1", "n2"})
	ef.waitIdle()

	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after silence ends = %d, want 2", ef.deliveredCount())
	}
	if title := ef.lastDelivered().Title; !strings.Contains(title, "box1") {
		t.Fatalf("delivered title after unsilence = %q, want box1", title)
	}
	inc, ok = ef.incidents.Get(id)
	if !ok || inc.State != "firing" {
		t.Fatalf("incident after unsilence = %+v, want firing", inc)
	}
	for _, al := range inc.Alerts {
		if al.SilencedBy != "" {
			t.Fatalf("member %+v still shows SilencedBy after its silence ended", al)
		}
	}
}

// TestEngineSilencedSiblingDoesNotFreezeEscalation: an unsilenced member fires
// first and starts escalating; a silenced sibling joining afterward must not
// disturb its escalation (a later fire must not overwrite the whole incident's
// State, since TickEscalations only considers state=="firing"). After the
// sibling's silence ends, it is delivered and the state is still "firing".
func TestEngineSilencedSiblingDoesNotFreezeEscalation(t *testing.T) {
	rf := newRoutingFixture(t, twoStepPolicy("5m", "10m")) // step 1 @5m, repeat every 10m
	rf.engine.PushLeaseNow("n1", rf.now)
	rf.engine.PushLeaseNow("n2", rf.now)
	names := map[string]string{"n1": "box1", "n2": "box2"}
	store := newTestSilenceStore(t)
	rf.engine.SetSilences(store, func(id string) (string, []string) { return names[id], nil })

	rf.engine.Submit(alertSource{NodeID: "n1", NodeName: "box1"}, Alert{Key: "cpu", Title: "cpu high", Severity: SevCritical, Kind: "fire", Time: rf.now.Unix()})
	rf.waitIdle()
	if rf.deliveredCount() != 1 {
		t.Fatalf("delivered after n1 fires = %d, want 1", rf.deliveredCount())
	}

	rf.advance(6 * time.Minute) // past step 1's 5m
	rf.tick()
	if rf.dispatchedCount() != 1 {
		t.Fatalf("dispatchedTo after step 1 due = %d, want 1 (n1's own escalation)", rf.dispatchedCount())
	}

	// A silence now matches n2 only; n2 fires and joins the SAME incident
	// (same rule/severity), silenced -- it must not disturb n1's ongoing
	// escalation or the incident's overall state.
	sil, err := store.Create(core.Silence{Matchers: []core.Matcher{{Node: "box2"}}, Start: 0, End: rf.now.Unix() + 3600, Author: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	rf.engine.Submit(alertSource{NodeID: "n2", NodeName: "box2"}, Alert{Key: "cpu", Title: "cpu high", Severity: SevCritical, Kind: "fire", Time: rf.now.Unix()})
	rf.waitIdle()

	inc := rf.onlyIncident(t)
	if inc.State != "firing" {
		t.Fatalf("incident after n2 (silenced) joins = %+v, want still firing (n1 active)", inc)
	}
	if rf.deliveredCount() != 1 {
		t.Fatalf("delivered after n2's (silenced) fire = %d, want still 1", rf.deliveredCount())
	}

	// n1's escalation keeps running: its RepeatEvery (10m, due at +16m) still
	// fires, keyed off n1 (latestActiveAlert must not switch to n2 just
	// because n2 fired more recently -- n2 is silenced, so it is never
	// "active").
	rf.now = rf.now.Add(10*time.Minute + time.Second) // total +16m1s since n1 fired
	rf.tick()
	if rf.dispatchedCount() != 2 {
		t.Fatalf("dispatchedTo after the repeat is due = %d, want 2 (n1's repeat notification)", rf.dispatchedCount())
	}

	// n2's silence ends: it is delivered alone, and the incident is (still)
	// firing.
	if err := store.Expire(sil.ID, rf.now.Unix()); err != nil {
		t.Fatal(err)
	}
	rf.now = rf.now.Add(time.Second)
	rf.engine.TickSilences(rf.now, []string{"n1", "n2"})
	rf.waitIdle()

	if rf.deliveredCount() != 2 {
		t.Fatalf("deliveredTo after n2's silence ends = %d, want 2 (n2's own delivery)", rf.deliveredCount())
	}
	inc = rf.onlyIncident(t)
	if inc.State != "firing" {
		t.Fatalf("incident after n2 unsilenced = %+v, want firing", inc)
	}
}

// --- CRITICAL 2: a folded dependent's own recover must find its real bucket

// TestEngineFoldedChildRecoverResolvesCorrectBucket: a child folded into its
// down parent's incident recovers on its own WHILE the parent is still
// down. Before the openMember index existed, Apply's recover branch found
// the incident to update by recomputing a group key for the recover
// itself -- the child's own DEFAULT bucket, which has nothing to do with
// the parent's incident it actually lives in -- so the fold entry was never
// actually resolved, and a LATER parent recover would find it still open
// and deliver a false "child down" re-fire for an alert that had already
// recovered. openMember fixes this: a recover locates its member by
// (node, key) directly, regardless of what bucket its own key/severity
// would otherwise compute.
func TestEngineFoldedChildRecoverResolvesCorrectBucket(t *testing.T) {
	dir := t.TempDir()
	incidents, err := loadIncidentStore(dir + "/incidents.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return now },
		func(a Alert) bool { mu.Lock(); delivered = append(delivered, a); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	names := map[string]string{"parent": "web-1", "child": "web-2"}
	engine.SetSilences(nil, func(id string) (string, []string) { return names[id], nil })
	engine.SetDependencies(func(id string) []string {
		if id == "child" {
			return []string{"parent"}
		}
		return nil
	})

	parentKey, childKey := nodeDownKey("parent"), nodeDownKey("child")

	engine.Submit(alertSource{}, Alert{Key: parentKey, Title: "web-1 down", Severity: SevCritical, Kind: "fire", Time: now.Unix()})
	engine.waitIdleForTest()
	engine.Submit(alertSource{}, Alert{Key: childKey, Title: "web-2 down", Severity: SevCritical, Kind: "fire", Time: now.Unix()})
	engine.waitIdleForTest()

	inc, ok := incidents.FindByAlertKey(parentKey)
	if !ok {
		t.Fatal("parent incident missing")
	}
	parentID := inc.ID

	mu.Lock()
	n := len(delivered)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered after both fires = %d, want 1 (only the parent; the child is folded)", n)
	}

	// The child recovers on its own WHILE the parent is still down.
	now = now.Add(time.Minute)
	engine.Submit(alertSource{}, Alert{Key: childKey, Title: "web-2 back", Severity: SevCritical, Kind: "recover", Time: now.Unix()})
	engine.waitIdleForTest()

	mu.Lock()
	n = len(delivered)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered after the folded child recovers = %d, want still 1 (nothing delivered for it)", n)
	}

	inc, ok = incidents.Get(parentID)
	if !ok {
		t.Fatal("parent incident missing after the child's recover")
	}
	var childAlert core.IncidentAlert
	found := false
	for _, al := range inc.Alerts {
		if al.Key == childKey {
			childAlert, found = al, true
		}
	}
	if !found || childAlert.ResolvedAt == 0 {
		t.Fatalf("child member = %+v found=%v, want resolved (openMember must find the parent's incident)", childAlert, found)
	}
	if inc.State == "resolved" {
		t.Fatalf("parent incident resolved early = %+v, want still open (the parent itself is still down)", inc)
	}

	// The parent recovers: no false re-fire for the already-recovered child,
	// and the parent incident resolves normally.
	now = now.Add(time.Minute)
	engine.Submit(alertSource{}, Alert{Key: parentKey, Title: "web-1 back", Severity: SevCritical, Kind: "recover", Time: now.Unix()})
	engine.waitIdleForTest()

	mu.Lock()
	got := append([]Alert(nil), delivered...)
	mu.Unlock()
	for _, a := range got {
		if a.Key == childKey {
			t.Fatalf("false re-fire for the already-recovered child: %+v", got)
		}
	}
	if len(got) != 2 || got[1].Kind != "recover" {
		t.Fatalf("delivered after the parent recovers = %+v, want exactly 2 (parent fire + parent recover)", got)
	}

	inc, ok = incidents.Get(parentID)
	if !ok {
		t.Fatal("parent incident missing after the parent's recover")
	}
	if inc.State != "resolved" {
		t.Fatalf("parent incident after both members recovered = %+v, want resolved", inc)
	}
}

// --- grouped resurrection -------------------------------------

// TestEngineResurrectionGroupsMultipleMastersOwnMembersIntoOneMessage: two
// master-own members of the SAME incident are both undelivered when the
// master "crashes" (recorded via direct Apply calls, never reaching
// delivery). Resurrection must send ONE grouped message listing both,
// rather than two separate solo redeliveries.
func TestEngineResurrectionGroupsMultipleMastersOwnMembersIntoOneMessage(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/incidents.jsonl"
	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	shared := groupKeyFor(nil, alertSource{}, Alert{Key: "fleet:node:a:down", Severity: SevCritical})
	if _, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:a:down", Title: "a is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000, groupKey: shared,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:b:down", Title: "b is down", Severity: SevCritical, Kind: "fire", Time: 1005},
		firedAt: 1005, now: 1005, groupKey: shared,
	}); err != nil {
		t.Fatal(err)
	}
	// Neither member's fire was ever recorded as delivered -- the "crash".

	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(1100, 0) },
		func(a Alert) bool { mu.Lock(); delivered = append(delivered, a); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents2,
	)
	engine.waitIdleForTest()

	mu.Lock()
	got := append([]Alert(nil), delivered...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("delivered on resurrection = %+v, want exactly ONE grouped message", got)
	}
	if !strings.Contains(got[0].Title, "2 nodes") {
		t.Fatalf("delivered title = %q, want it to list both members (2 nodes)", got[0].Title)
	}

	// Both members' fires must now show as delivered.
	inc, ok := incidents2.FindByAlertKey("fleet:node:a:down")
	if !ok {
		t.Fatal("incident missing")
	}
	for _, key := range []string{"fleet:node:a:down", "fleet:node:b:down"} {
		var al core.IncidentAlert
		for _, cand := range inc.Alerts {
			if cand.Key == key {
				al = cand
			}
		}
		if fireDelivered, _ := legDeliveredStatusFor(inc, "", key, al.FiredAt); !fireDelivered {
			t.Fatalf("member %s not marked delivered after grouped resurrection: %+v", key, inc.Timeline)
		}
	}
}

// --- SetNodeDeps must release a folded member itself ---------

// TestSetNodeDepsReleasesFoldedChildImmediately: a child is folded into its
// down parent's incident; removing the dependency (not waiting for the
// parent to recover, which never happens in this test) must release and
// promptly deliver the still-down child. Runs against a REAL master
// (startFleet), since SetNodeDeps only exists on fleetAPIImpl/masterState.
func TestSetNodeDepsReleasesFoldedChildImmediately(t *testing.T) {
	dir := t.TempDir()
	d, alerts := testDeps(t, dir)
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Fleet.Role = config.RoleMaster
	cfg.Fleet.Address = "127.0.0.1"
	cfg.Fleet.Listen = "127.0.0.1:0"
	rt := startFleet(context.Background(), cfg, d)
	t.Cleanup(rt.stop)
	m := rt.provider.master
	if m == nil {
		t.Fatal("master did not start")
	}
	fa := rt.provider.Fleet()

	now := time.Now().Unix()
	parent := addNode(t, m, "parent", now)
	child := addNode(t, m, "child", now)

	if err := fa.SetNodeDeps(child, []string{parent}, "op"); err != nil {
		t.Fatal(err)
	}

	parentKey, childKey := nodeDownKey(parent), nodeDownKey(child)
	m.engine.Submit(alertSource{}, Alert{Key: parentKey, Title: "parent down", Severity: SevCritical, Kind: "fire", Time: now})
	m.engine.waitIdleForTest()
	m.engine.Submit(alertSource{}, Alert{Key: childKey, Title: "child down", Severity: SevCritical, Kind: "fire", Time: now})
	m.engine.waitIdleForTest()

	if len(*alerts) != 1 {
		t.Fatalf("delivered after both fires = %+v, want 1 (child folded into the parent's incident)", *alerts)
	}

	// Remove the dependency (the parent is STILL down -- nothing ever
	// recovers): the still-down child must be released and delivered
	// promptly, not stranded waiting for a recover that will never come.
	if err := fa.SetNodeDeps(child, nil, "op"); err != nil {
		t.Fatal(err)
	}
	m.engine.waitIdleForTest()

	if len(*alerts) != 2 {
		t.Fatalf("delivered after removing the dependency = %+v, want 2 (parent + released child)", *alerts)
	}
	found := false
	for _, a := range (*alerts)[1:] {
		if a.Key == childKey {
			found = true
		}
	}
	if !found {
		t.Fatalf("no delivery for the released child: %+v", *alerts)
	}
}
