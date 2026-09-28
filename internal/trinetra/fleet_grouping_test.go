// Package trinetra: fleet_grouping_test.go covers task 6 part 2 (incident
// grouping) and part 3 (node dependencies) end to end, through the engine's
// real Submit/TickGrouping/SetDependencies surface -- see fleet_engine.go's
// groupKeyFor/tryDeliverGroup/dependencyFoldReason/releaseFoldedDependents.
package trinetra

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// setGroupTimingForTest sets groupWait/groupInterval for the duration of a
// test, restoring the previous values (TestMain's 0, 0 baseline, unless a
// caller nests this) via t.Cleanup -- see TestMain's doc comment.
func setGroupTimingForTest(t *testing.T, wait, interval time.Duration) {
	t.Helper()
	prevWait, prevInterval := groupWait, groupInterval
	groupWait, groupInterval = wait, interval
	t.Cleanup(func() { groupWait, groupInterval = prevWait, prevInterval })
}

// TestGroupingTwoNodesWithinWaitProduceOneDelivery is the brief's first
// required grouping test: two nodes firing the same rule/severity within
// group_wait join ONE incident, and its first notification -- sent only
// once group_wait has elapsed since the incident opened -- lists both.
func TestGroupingTwoNodesWithinWaitProduceOneDelivery(t *testing.T) {
	ef := newEngineFixture(t)
	setGroupTimingForTest(t, 30*time.Second, 5*time.Minute)
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", ef.now)
	ef.engine.PushLeaseNow("n2", ef.now)

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()

	ef.now = ef.now.Add(10 * time.Second) // still within the 30s group_wait
	ef.engine.HandleChildAlert("n2", "box2", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered before group_wait elapsed = %d, want 0", ef.deliveredCount())
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || len(incs[0].Alerts) != 2 {
		t.Fatalf("incidents = %+v, want one incident with 2 members", incs)
	}

	ef.now = ef.now.Add(21 * time.Second) // 31s since the FIRST fire
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered after group_wait elapsed = %d, want 1", ef.deliveredCount())
	}
	title := ef.lastDelivered().Title
	if !strings.Contains(title, "box1") || !strings.Contains(title, "box2") {
		t.Fatalf("title = %q, want it to list both member nodes", title)
	}
	for _, node := range []string{"n1", "n2"} {
		if len(ef.framesFor(node, "receipt")) != 1 {
			t.Fatalf("node %s receipts = %d, want 1", node, len(ef.framesFor(node, "receipt")))
		}
	}
	// task 9: a grouped fire notification carries Ack/Silence buttons for
	// its own incident too, exactly like an ungrouped one.
	want := incidentButtons(incs[0].ID)
	if got := ef.lastDelivered().Buttons; !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons = %+v, want %+v", got, want)
	}
}

// TestGroupingThirdMemberAfterDeliveryUpdatesAtNextInterval is the brief's
// second required grouping test: a member joining AFTER the first delivery
// produces exactly one "update" notification, sent only once group_interval
// has elapsed since the last group delivery.
func TestGroupingThirdMemberAfterDeliveryUpdatesAtNextInterval(t *testing.T) {
	ef := newEngineFixture(t)
	setGroupTimingForTest(t, 30*time.Second, 5*time.Minute)
	// This test exercises groupInterval itself (5m), in isolation: task 6 fix
	// round 1's fallback_after cap (effectiveGroupInterval) would otherwise
	// shrink it to fallback_after/2 (1m by default) since every pending
	// member here is child-sourced -- see TestGroupingLateChildUpdateRespectsFallbackCap
	// for that interaction specifically.
	ef.engine.SetConfig(func() *config.Config {
		c := config.Default()
		c.Fleet.FallbackAfter = "24h"
		return c
	})
	ef.connect("n1")
	ef.connect("n2")
	ef.connect("n3")
	for _, n := range []string{"n1", "n2", "n3"} {
		ef.engine.PushLeaseNow(n, ef.now)
	}

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	ef.now = ef.now.Add(30 * time.Second)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("initial delivered = %d, want 1", ef.deliveredCount())
	}

	// A second member joins AFTER the first delivery.
	ef.now = ef.now.Add(time.Minute)
	ef.engine.HandleChildAlert("n2", "box2", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered before group_interval elapsed = %d, want still 1", ef.deliveredCount())
	}

	// A third member joins, still within group_interval of the first
	// delivery: still no update yet.
	ef.now = ef.now.Add(2 * time.Minute)
	ef.engine.HandleChildAlert("n3", "box3", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered before group_interval elapsed (2nd check) = %d, want still 1", ef.deliveredCount())
	}

	// group_interval (5m) has now elapsed since the FIRST (only) delivery:
	// ONE update, listing both new members.
	ef.now = ef.now.Add(3 * time.Minute)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()

	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after group_interval elapsed = %d, want 2 (initial + one update)", ef.deliveredCount())
	}
	update := ef.lastDelivered().Title
	if strings.Contains(update, "box1") {
		t.Fatalf("update title = %q, must list only the NEW members, not box1 again", update)
	}
	if !strings.Contains(update, "box2") || !strings.Contains(update, "box3") {
		t.Fatalf("update title = %q, want it to list box2 and box3", update)
	}

	// A further tick with nothing new pending must not deliver again.
	ef.now = ef.now.Add(time.Hour)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after a further idle tick = %d, want still 2", ef.deliveredCount())
	}
}

// TestGroupingAckedIncidentStillDeliversNewMemberUpdate pins a B6 review
// regression: acking a grouped incident must not suppress a member that
// fires LATER from getting its own update notification. tryDeliverGroup
// only ever excludes inc.State == "suppressed" (every open member silenced
// or dependency-folded) -- "acked" only stops future ESCALATION
// (tryEscalate gates on inc.State != "firing"), never a fresh member's
// first delivery through the group. This test fails if that ever regresses
// (e.g. tryDeliverGroup grows an "acked" exclusion mirroring tryEscalate's).
func TestGroupingAckedIncidentStillDeliversNewMemberUpdate(t *testing.T) {
	ef := newEngineFixture(t)
	setGroupTimingForTest(t, 30*time.Second, 5*time.Minute)
	// Same rationale as TestGroupingThirdMemberAfterDeliveryUpdatesAtNextInterval:
	// isolate group_interval itself from the fallback_after cap.
	ef.engine.SetConfig(func() *config.Config {
		c := config.Default()
		c.Fleet.FallbackAfter = "24h"
		return c
	})
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", ef.now)
	ef.engine.PushLeaseNow("n2", ef.now)

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	ef.now = ef.now.Add(30 * time.Second)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("initial delivered = %d, want 1", ef.deliveredCount())
	}

	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %+v, want 1", incs)
	}
	incID := incs[0].ID
	if _, err := ef.incidents.Ack(incID, "operator", ef.now.Unix()); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if inc, ok := ef.incidents.Get(incID); !ok || inc.State != "acked" {
		t.Fatalf("incident state after ack = %+v, want acked", inc)
	}

	// A second member fires AFTER the incident was acked.
	ef.now = ef.now.Add(time.Minute)
	ef.engine.HandleChildAlert("n2", "box2", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered right after the new member joined an acked incident = %d, want still 1 (group_interval hasn't elapsed yet)", ef.deliveredCount())
	}

	// group_interval (5m) elapses since the first (only) delivery: box2's
	// update must still go out even though the incident is acked.
	ef.now = ef.now.Add(5 * time.Minute)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()

	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after an acked incident gained a new member = %d, want 2 (initial fire + the new member's update)", ef.deliveredCount())
	}
	update := ef.lastDelivered().Title
	if !strings.Contains(update, "box2") {
		t.Fatalf("update title = %q, want it to list box2", update)
	}
	if strings.Contains(update, "box1") {
		t.Fatalf("update title = %q, must list only the NEW member, not box1 again", update)
	}
	if inc, ok := ef.incidents.Get(incID); !ok || inc.State != "acked" {
		t.Fatalf("incident state after delivering the new member's update = %+v, want still acked (delivering must not un-ack it)", inc)
	}
}

// TestGroupingLateChildUpdateRespectsFallbackCap is task 6 fix round 1's
// IMPORTANT 5 required test: with the DEFAULT config (fleet.fallback_after
// 2m, so effectiveGroupInterval caps at 1m for a child-sourced pending
// member) and the spec's default group_interval (5m, which would otherwise
// leave a late-joining child waiting far longer than its own local fallback
// timer), a late child member's update ships within 60s of the incident's
// last group delivery -- not up to 5 minutes later.
func TestGroupingLateChildUpdateRespectsFallbackCap(t *testing.T) {
	ef := newEngineFixture(t)
	setGroupTimingForTest(t, 30*time.Second, 5*time.Minute) // spec defaults
	ef.engine.SetConfig(func() *config.Config { return config.Default() })
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", ef.now)
	ef.engine.PushLeaseNow("n2", ef.now)

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	ef.now = ef.now.Add(30 * time.Second) // group_wait elapses: first delivery.
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("initial delivered = %d, want 1", ef.deliveredCount())
	}
	lastDelivery := ef.now

	// A late child member joins after the first delivery.
	ef.now = ef.now.Add(10 * time.Second)
	ef.engine.HandleChildAlert("n2", "box2", nil, AlertEvent{
		Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: ef.now.Unix(),
	})
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered right after n2 joins = %d, want still 1", ef.deliveredCount())
	}

	// Just under the 60s (fallback_after/2) cap since the last delivery: no
	// update yet -- proves this is the ACTIVE gate, not an immediate/no-op one.
	ef.now = lastDelivery.Add(59 * time.Second)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered at 59s since the last delivery = %d, want still 1", ef.deliveredCount())
	}

	// Just past the 60s cap: the update ships -- well before the raw 5m
	// group_interval would otherwise have allowed.
	ef.now = lastDelivery.Add(61 * time.Second)
	ef.engine.TickGrouping(ef.now)
	ef.waitIdle()
	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered at 61s since the last delivery = %d, want 2 (the capped update shipped)", ef.deliveredCount())
	}
	if title := ef.lastDelivered().Title; !strings.Contains(title, "box2") {
		t.Fatalf("update title = %q, want it to list box2", title)
	}
}

// TestGroupingRouteGroupByNodeKeepsNodesSeparate is the brief's third
// required grouping test: a route's GroupBy overrides the default (rule,
// severity) bucket -- GroupBy: ["node"] means two different nodes never
// share an incident even for the identical rule/severity.
func TestGroupingRouteGroupByNodeKeepsNodesSeparate(t *testing.T) {
	sendResolved := true
	cfg := core.AlertingConfig{
		Policies: []core.Policy{{
			Name:         "p",
			Steps:        []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}},
			SendResolved: &sendResolved,
		}},
		Routes: []core.Route{{
			Name:     "by-node",
			Matchers: []core.Matcher{{Rule: "cpu"}},
			Policy:   "p",
			GroupBy:  []string{"node"},
		}},
		DefaultPolicy: "p",
	}
	rf := newRoutingFixture(t, cfg)
	// A Submit call carries its own alertSource (unlike HandleChildAlert,
	// which fills DeliveredLocally/RoutedToMaster from the wire record):
	// without a pushed lease, hadLeaseBefore reports false and Submit would
	// treat this as already delivered locally by the child.
	rf.engine.PushLeaseNow("n1", rf.now)
	rf.engine.PushLeaseNow("n2", rf.now)

	rf.engine.Submit(alertSource{NodeID: "n1", NodeName: "box1"}, Alert{Key: "cpu", Title: "cpu critical", Severity: SevCritical, Kind: "fire", Time: rf.now.Unix()})
	rf.engine.Submit(alertSource{NodeID: "n2", NodeName: "box2"}, Alert{Key: "cpu", Title: "cpu critical", Severity: SevCritical, Kind: "fire", Time: rf.now.Unix()})
	rf.waitIdle()

	incs := rf.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 2 {
		t.Fatalf("incidents = %+v, want 2 (GroupBy node keeps them separate)", incs)
	}
	for _, inc := range incs {
		if len(inc.Alerts) != 1 {
			t.Fatalf("incident %s has %d members, want 1 each", inc.ID, len(inc.Alerts))
		}
	}
	if rf.deliveredCount() != 2 {
		t.Fatalf("deliveredTo = %d, want 2 (one delivery per node)", rf.deliveredCount())
	}
}

// TestGroupingMemberOrderingAndIncidentStaysOpenUntilAllRecover is the
// brief's fourth required grouping test: within one incident, member A's own
// fire is delivered before its own recover is ever considered (per-alert
// ordering holds even though member B joins in between), and the incident
// stays open -- no "resolved" notification -- until member B ALSO recovers.
func TestGroupingMemberOrderingAndIncidentStaysOpenUntilAllRecover(t *testing.T) {
	ef := newEngineFixture(t) // group_wait/interval are 0 here: immediate grouped delivery.
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", ef.now)
	ef.engine.PushLeaseNow("n2", ef.now)

	// Every call advances the fake clock first, so each alert record gets a
	// distinct FiredAt -- the master's dedup key is (node, key, fired_at);
	// reusing the same instant for a member's fire AND its own recover would
	// collide with its own fire's dedup entry and be silently dropped.
	fire := func(node, name string) {
		ef.now = ef.now.Add(10 * time.Second)
		ef.engine.HandleChildAlert(node, name, nil, AlertEvent{
			Time: ef.now.Unix(), Key: "cpu", Title: "cpu critical", Severity: "critical", Kind: "fire", Source: "anomaly",
			RoutedToMaster: true, FiredAt: ef.now.Unix(),
		})
	}
	recover := func(node, name string) {
		ef.now = ef.now.Add(10 * time.Second)
		ef.engine.HandleChildAlert(node, name, nil, AlertEvent{
			Time: ef.now.Unix(), Key: "cpu", Title: "cpu back to normal", Severity: "critical", Kind: "recover", Source: "anomaly",
			RoutedToMaster: true, FiredAt: ef.now.Unix(),
		})
	}

	fire("n1", "box1")
	ef.waitIdle()
	fire("n2", "box2")
	ef.waitIdle()
	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after both fires = %d, want 2 (initial + immediate update)", ef.deliveredCount())
	}

	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %+v, want 1 (both members joined the same incident)", incs)
	}
	id := incs[0].ID
	receiptsBefore := len(ef.framesFor("n1", "receipt"))

	// Member A recovers: no NEW human notification (the incident is not
	// over -- B is still firing), but the incident stays open, and A's own
	// receipt still goes out (so A's child does not fall back locally).
	recover("n1", "box1")
	ef.waitIdle()
	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered after A's recover = %d, want still 2 (no notification for a partial recover)", ef.deliveredCount())
	}
	inc, ok := ef.incidents.Get(id)
	if !ok || inc.State == "resolved" {
		t.Fatalf("incident after A's recover = %+v, want still open", inc)
	}
	for _, al := range inc.Alerts {
		if al.Node == "n1" && al.ResolvedAt == 0 {
			t.Fatalf("A's own member never recorded as resolved: %+v", al)
		}
	}
	if got := len(ef.framesFor("n1", "receipt")); got <= receiptsBefore {
		t.Fatalf("A's own recover never got a NEW receipt (before=%d after=%d)", receiptsBefore, got)
	}

	// Member B recovers too: NOW the incident is fully resolved, and exactly
	// one final "resolved" notification goes out.
	recover("n2", "box2")
	ef.waitIdle()
	if ef.deliveredCount() != 3 {
		t.Fatalf("delivered after B's recover = %d, want 3 (final resolved notification)", ef.deliveredCount())
	}
	if got := ef.delivered[2].Kind; got != "recover" {
		t.Fatalf("final delivery kind = %q, want recover", got)
	}
	inc, ok = ef.incidents.Get(id)
	if !ok || inc.State != "resolved" {
		t.Fatalf("incident after both recover = %+v, want resolved", inc)
	}
}

// TestEngineResurrectionOnlyResendsUndeliveredMember is the brief's fifth
// required test: a grouped incident with two members where only ONE
// member's fire was ever recorded as delivered before a "crash" must, on
// resurrection, resend only the OTHER member.
func TestEngineResurrectionOnlyResendsUndeliveredMember(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/incidents.jsonl"
	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	shared := groupKeyFor(nil, alertSource{}, Alert{Key: "fleet:node:a:down", Severity: SevCritical})

	incA, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:a:down", Title: "a is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000, groupKey: shared,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incidents1.AppendEvent(incA.ID, core.IncidentEvent{
		TS: 1010, Kind: "delivered", Detail: "fire: sent via the master's dispatcher (grouped)", Actor: "system",
		Leg: "fire", AlertKey: "fleet:node:a:down", Node: "", FiredAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:b:down", Title: "b is down", Severity: SevCritical, Kind: "fire", Time: 1005},
		firedAt: 1005, now: 1005, groupKey: shared,
	}); err != nil {
		t.Fatal(err)
	}
	// b's own fire was never recorded as delivered -- the "crash".

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
	defer mu.Unlock()
	if len(delivered) != 1 || delivered[0].Key != "fleet:node:b:down" {
		t.Fatalf("delivered on resurrection = %+v, want exactly one redelivery for fleet:node:b:down", delivered)
	}
}

// TestEngineDependencyFoldAndRelease is the brief's required dependency
// test: a child node's node-down alert folds silently into its down
// parent's incident (one delivery, the parent's), and is delivered
// separately, as its own new incident, once the parent recovers while the
// child is still down.
func TestEngineDependencyFoldAndRelease(t *testing.T) {
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

	// Parent goes down: delivered normally.
	engine.Submit(alertSource{}, Alert{Key: parentKey, Title: "🔴 web-1 is down", Severity: SevCritical, Kind: "fire", Time: now.Unix()})
	engine.waitIdleForTest()
	mu.Lock()
	n := len(delivered)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered after parent down = %d, want 1", n)
	}

	// Child goes down while the parent is still down: folded, NOT delivered
	// separately.
	engine.Submit(alertSource{}, Alert{Key: childKey, Title: "🔴 web-2 is down", Severity: SevCritical, Kind: "fire", Time: now.Unix()})
	engine.waitIdleForTest()
	mu.Lock()
	n = len(delivered)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("delivered after child down (should fold) = %d, want still 1", n)
	}
	inc, ok := incidents.FindByAlertKey(parentKey)
	if !ok {
		t.Fatal("parent incident missing")
	}
	var childAlert core.IncidentAlert
	found := false
	for _, al := range inc.Alerts {
		if al.Key == childKey {
			childAlert, found = al, true
		}
	}
	if !found || childAlert.Suppressed != "suppressed: parent web-1 down" {
		t.Fatalf("child member = %+v found=%v, want folded with reason 'suppressed: parent web-1 down'", childAlert, found)
	}

	// Parent recovers while the child is still down: the child is released
	// and delivered, as its own incident. now must advance first -- the
	// dedup key is (node, key, fired_at), and a recover sharing its own
	// fire's exact instant would collide with that fire's own dedup entry.
	now = now.Add(time.Minute)
	engine.Submit(alertSource{}, Alert{Key: parentKey, Title: "web-1 is back", Severity: SevCritical, Kind: "recover", Time: now.Unix()})
	engine.waitIdleForTest()

	mu.Lock()
	got := append([]Alert(nil), delivered...)
	mu.Unlock()
	var releasedDelivery bool
	for _, a := range got {
		if a.Key == childKey && a.Kind == "fire" {
			releasedDelivery = true
		}
	}
	if !releasedDelivery {
		t.Fatalf("delivered after parent recovers = %+v, want a fire delivery for %s", got, childKey)
	}

	parentInc, ok := incidents.Get(inc.ID)
	if !ok {
		t.Fatal("parent incident missing after recover")
	}
	for _, al := range parentInc.Alerts {
		if al.Key == childKey && !strings.HasPrefix(al.Suppressed, "released:") {
			t.Fatalf("child's old fold entry = %+v, want marked released", al)
		}
	}

	// The child's release must have opened a SEPARATE incident (never the
	// parent's own) carrying an unresolved, un-suppressed childKey member.
	var childIncFound bool
	for _, other := range incidents.List(core.IncidentFilter{}, nil) {
		if other.ID == inc.ID {
			continue
		}
		for _, al := range other.Alerts {
			if al.Key == childKey && al.Suppressed == "" && al.ResolvedAt == 0 {
				childIncFound = true
			}
		}
	}
	if !childIncFound {
		t.Fatalf("no separate, unresolved, un-suppressed incident found for %s after release", childKey)
	}
}
