package trinetra

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// TestMain zeroes groupWait/groupInterval for this package's ENTIRE test binary run: every
// engine/daemon test that predates incident grouping fires an alert.
func TestMain(m *testing.M) {
	groupWait, groupInterval = 0, 0
	os.Exit(m.Run())
}

// engineFixture wires a fleetAlertEngine over a real incidentStore.
type engineFixture struct {
	mu          sync.Mutex
	delivered   []Alert
	pushed      []pushedFrame
	connected   map[string]bool
	deliverFail bool // when true, deliver reports failure (no channel accepted it)

	incidents *incidentStore
	engine    *fleetAlertEngine
	now       time.Time
}

type pushedFrame struct {
	node string
	f    fleet.Frame
}

// disableGroupWaitForTest zeroes groupWait/groupInterval for the duration of a test
// (restored via t.Cleanup).
func disableGroupWaitForTest(t *testing.T) {
	t.Helper()
	prevWait, prevInterval := groupWait, groupInterval
	groupWait, groupInterval = 0, 0
	t.Cleanup(func() { groupWait, groupInterval = prevWait, prevInterval })
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	disableGroupWaitForTest(t)
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ef := &engineFixture{
		connected: map[string]bool{},
		incidents: incidents,
		now:       time.Unix(1_700_000_000, 0),
	}
	ef.engine = newFleetAlertEngine(
		func() time.Time { return ef.now },
		func(a Alert) bool {
			ef.mu.Lock()
			fail := ef.deliverFail
			if !fail {
				ef.delivered = append(ef.delivered, a)
			}
			ef.mu.Unlock()
			return !fail
		},
		func(node string, f fleet.Frame) bool {
			ef.mu.Lock()
			defer ef.mu.Unlock()
			if !ef.connected[node] {
				return false
			}
			ef.pushed = append(ef.pushed, pushedFrame{node, f})
			return true
		},
		func(node string) bool { ef.mu.Lock(); defer ef.mu.Unlock(); return ef.connected[node] },
		incidents,
	)
	return ef
}

// waitIdle blocks until every delivery goroutine Submit has spawned so far
// has finished, so assertions on delivered/pushed are deterministic.
func (ef *engineFixture) waitIdle() { ef.engine.waitIdleForTest() }

func (ef *engineFixture) connect(node string) {
	ef.mu.Lock()
	ef.connected[node] = true
	ef.mu.Unlock()
}

func (ef *engineFixture) disconnect(node string) {
	ef.mu.Lock()
	delete(ef.connected, node)
	ef.mu.Unlock()
}

func (ef *engineFixture) deliveredCount() int {
	ef.mu.Lock()
	defer ef.mu.Unlock()
	return len(ef.delivered)
}

func (ef *engineFixture) lastDelivered() Alert {
	ef.mu.Lock()
	defer ef.mu.Unlock()
	return ef.delivered[len(ef.delivered)-1]
}

func (ef *engineFixture) framesFor(node, typ string) []fleet.Frame {
	ef.mu.Lock()
	defer ef.mu.Unlock()
	var out []fleet.Frame
	for _, p := range ef.pushed {
		if p.node == node && p.f.Type == typ {
			out = append(out, p.f)
		}
	}
	return out
}

func TestEngineChildFireEnrichesDeliversAndReceipts(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0)) // lease held well before the alert fires

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, ev)
	ef.waitIdle() // delivery (Submit steps 2-4) runs off its own goroutine

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered count = %d, want 1", ef.deliveredCount())
	}
	if got := ef.lastDelivered().Title; got != "box1: cpu high" {
		t.Fatalf("title = %q, want %q", got, "box1: cpu high")
	}
	receipts := ef.framesFor("n1", "receipt")
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d, want 1", len(receipts))
	}
	var rd receiptFrameData
	if err := json.Unmarshal(receipts[0].Data, &rd); err != nil || rd.Key != "cpu" || rd.FiredAt != 1000 {
		t.Fatalf("receipt data = %+v err %v", rd, err)
	}

	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incs))
	}
	inc := incs[0]
	if inc.State != "firing" || len(inc.Alerts) != 1 || inc.Alerts[0].Node != "n1" || inc.Alerts[0].Key != "cpu" || inc.Alerts[0].DeliveredLocally {
		t.Fatalf("incident = %+v", inc)
	}
	foundDelivered := false
	for _, e := range inc.Timeline {
		if e.Kind == "delivered" {
			foundDelivered = true
		}
	}
	if !foundDelivered {
		t.Fatalf("timeline missing a delivered event: %+v", inc.Timeline)
	}
}

// TestMarkDeliveredLocallyScopesToItsOwnMemberNotWholeIncident reproduces the docker fleet
// e2e's step 12 failure end to end: n1's cpu incident is opened, delivered once.
func TestMarkDeliveredLocallyScopesToItsOwnMemberNotWholeIncident(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.engine.PushLeaseNow("n2", time.Unix(500, 0))

	// n1's cpu fires, delivers once, and is left open forever.
	ef.engine.HandleChildAlert("n1", "box1", []string{"web"}, AlertEvent{
		Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning",
		Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000,
	})
	ef.waitIdle()
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents after n1 fire = %d, want 1", len(incs))
	}
	if got := incs[0].GroupKey; got != "rule=cpu|severity=warning" {
		t.Fatalf("group key = %q, want the default node-less bucket", got)
	}

	// n2's cpu fires (T_A) -- delivered normally via the shared incident's group delivery,
	// THEN a second, byte-identical-dedup-key record for the SAME.
	tAFire := AlertEvent{Time: 2000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 2000}
	ef.engine.HandleChildAlert("n2", "box2", []string{"web"}, tAFire)
	ef.waitIdle()
	tAFireDeliveredLocally := tAFire
	tAFireDeliveredLocally.DeliveredLocally = true
	ef.engine.HandleChildAlert("n2", "box2", []string{"web"}, tAFireDeliveredLocally)
	ef.waitIdle()

	// T_A recovers.
	ef.engine.HandleChildAlert("n2", "box2", []string{"web"}, AlertEvent{
		Time: 2030, Key: "cpu", Title: "cpu back to normal", Severity: "warning",
		Kind: "recover", Source: "anomaly", RoutedToMaster: true, FiredAt: 2030,
	})
	ef.waitIdle()

	beforeCount := ef.deliveredCount()

	// T_B: n2's cpu fires again (a genuinely new, unresolved member) --
	// must still be delivered by the shared incident's group delivery.
	ef.engine.HandleChildAlert("n2", "box2", []string{"web"}, AlertEvent{
		Time: 3000, Key: "cpu", Title: "cpu high again", Severity: "warning",
		Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 3000,
	})
	ef.waitIdle()

	if got := ef.deliveredCount(); got != beforeCount+1 {
		t.Fatalf("delivered count after T_B's fire = %d, want %d (T_B must still be delivered, not silently swallowed by T_A's MarkDeliveredLocally event): all delivered=%+v",
			got, beforeCount+1, ef.delivered)
	}
	if got := ef.lastDelivered().Title; got != "box2: cpu high again" {
		t.Fatalf("last delivered title = %q, want T_B's own title", got)
	}
}

// TestEngineFireCarriesIncidentButtons pins that a delivered fire Alert carries
// Ack/Silence-1h buttons naming its own incident's id.
func TestEngineFireCarriesIncidentButtons(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})
	ef.waitIdle()

	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1", len(incs))
	}
	want := incidentButtons(incs[0].ID)
	if got := ef.lastDelivered().Buttons; !reflect.DeepEqual(got, want) {
		t.Fatalf("buttons = %+v, want %+v", got, want)
	}
}

// TestEngineRecoverCarriesNoButtons: a recover notification never carries
// Ack/Silence buttons (only incident fire messages do).
func TestEngineRecoverCarriesNoButtons(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})
	ef.waitIdle()
	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1050, Key: "cpu", Title: "cpu normal", Severity: "warning", Kind: "recover", Source: "anomaly", RoutedToMaster: true, FiredAt: 1050})
	ef.waitIdle()

	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered count = %d, want 2 (fire + recover)", ef.deliveredCount())
	}
	if got := ef.lastDelivered().Buttons; got != nil {
		t.Fatalf("recover carried buttons: %+v", got)
	}
}

func TestEngineDedupsSameAlertArrivingTwice(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	// Same (node, key, fired_at) arriving twice -- e.g. once via Backfill,
	// once via Ingest -- must be processed (delivered, receipted) only once.
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)
	ef.waitIdle()

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered count = %d, want 1", ef.deliveredCount())
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 1 {
		t.Fatalf("receipts = %d, want 1", got)
	}
	if got := len(ef.incidents.List(core.IncidentFilter{}, nil)); got != 1 {
		t.Fatalf("incidents = %d, want 1", got)
	}
}

func TestEngineDeliveredLocallyRecordIsNeverRedelivered(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", ef.now.Add(-time.Minute))

	// The fallback record: RoutedToMaster stays false (deliverFallback never sets it),
	// DeliveredLocally true, FiredAt carries the ORIGINAL fire's time.
	ev := AlertEvent{Time: 1200, Key: "cpu", Title: "via local fallback: master unreachable — cpu high",
		Severity: "warning", Kind: "fire", Source: "anomaly", DeliveredLocally: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none (already delivered locally)", ef.delivered)
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 0 {
		t.Fatalf("receipts pushed = %d, want 0", got)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || !incs[0].Alerts[0].DeliveredLocally {
		t.Fatalf("incident = %+v", incs)
	}
}

func TestEngineRoutedToMasterFalseIsAlreadyDeliveredLocally(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", ef.now.Add(-time.Minute))

	// RoutedToMaster false (the child had no valid lease at fire time and delivered locally
	// then and there) but DeliveredLocally is the zero-value false too.
	ev := AlertEvent{Time: 1000, Key: "mem", Title: "mem high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: false, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none", ef.delivered)
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 0 {
		t.Fatalf("receipts pushed = %d, want 0", got)
	}
}

func TestEngineNodeNeverHeldLeaseOverridesRoutedToMaster(t *testing.T) {
	ef := newEngineFixture(t)
	// n1 is connected, but never got a SUCCESSFUL lease push (no PushLeaseNow call, no
	// TickLeases pass touched it): even though this record claims RoutedToMaster true.
	ef.connect("n1")

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none (node never held a lease)", ef.delivered)
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 0 {
		t.Fatalf("receipts pushed = %d, want 0", got)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || !incs[0].Alerts[0].DeliveredLocally {
		t.Fatalf("incident = %+v", incs)
	}
}

func TestEngineLeaseAfterFireStillCountsAsNeverHeld(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	// The node's FIRST successful lease happens AFTER this alert's fired_at: from the master's
	// perspective it had not yet given this node a lease when the alert fired.
	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.PushLeaseNow("n1", time.Unix(1001, 0))
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none", ef.delivered)
	}
}

func TestEngineRecoverResolvesIncidentAndDeliversByDefault(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))

	fire := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	ef.engine.HandleChildAlert("n1", "box1", nil, fire)
	ef.waitIdle()

	recov := AlertEvent{Time: 1050, Key: "cpu", Title: "cpu back to normal", Severity: "warning", Kind: "recover", Source: "anomaly", RoutedToMaster: true, FiredAt: 1050}
	ef.engine.HandleChildAlert("n1", "box1", nil, recov)
	ef.waitIdle()

	if ef.deliveredCount() != 2 {
		t.Fatalf("delivered count = %d, want 2 (fire + recover)", ef.deliveredCount())
	}
	if got := ef.lastDelivered().Title; got != "box1: cpu back to normal" {
		t.Fatalf("recover title = %q", got)
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 {
		t.Fatalf("incidents = %d, want 1 (fire+recover share one incident)", len(incs))
	}
	inc := incs[0]
	if inc.State != "resolved" || inc.Resolved != ef.now.Unix() {
		t.Fatalf("incident = %+v", inc)
	}
	if len(inc.Alerts) != 1 || inc.Alerts[0].ResolvedAt != 1050 {
		t.Fatalf("incident alerts = %+v", inc.Alerts)
	}
	receipts := ef.framesFor("n1", "receipt")
	if len(receipts) != 2 {
		t.Fatalf("receipts = %d, want 2 (one per fire/recover)", len(receipts))
	}
}

func TestEngineMasterOwnAlertGoesThroughSubmitUnprefixed(t *testing.T) {
	ef := newEngineFixture(t)
	a := Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Source: "fleet", Time: 500}
	ef.engine.Submit(alertSource{}, a)
	ef.waitIdle()

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered count = %d, want 1", ef.deliveredCount())
	}
	if got := ef.lastDelivered().Title; got != "x is down" {
		t.Fatalf("title = %q, want unprefixed %q", got, "x is down")
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	wantGroupKey := groupKeyFor(nil, alertSource{}, a)
	if len(incs) != 1 || incs[0].GroupKey != wantGroupKey || len(incs[0].Nodes) != 0 {
		t.Fatalf("incident = %+v, want group key %q", incs, wantGroupKey)
	}
}

// --- master-own alert crash recovery -------------

// TestEngineResurrectsMasterOwnAlertAfterCrash: the master records a node-down fire
// (Submit's step 1) and then "crashes" before any delivery goroutine ever ran.
func TestEngineResurrectsMasterOwnAlertAfterCrash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	inc0, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(1100, 0) }, // well within 24h of the fire
		func(a Alert) bool { mu.Lock(); delivered = append(delivered, a); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents2,
	)
	engine.waitIdleForTest()

	mu.Lock()
	gotDelivered := append([]Alert(nil), delivered...)
	mu.Unlock()
	if len(gotDelivered) != 1 || gotDelivered[0].Key != "fleet:node:x:down" || gotDelivered[0].Title != "x is down" {
		t.Fatalf("delivered = %+v, want exactly one redelivery", gotDelivered)
	}
	inc, ok := incidents2.Get(inc0.ID)
	if !ok {
		t.Fatal("incident missing after resurrection")
	}
	// A fire-leg resurrection now goes through the same grouped-delivery path
	// (tryDeliverGroup) an ordinary fire would.
	last := inc.Timeline[len(inc.Timeline)-1]
	if last.Kind != "delivered" || !strings.HasPrefix(last.Detail, "fire: ") {
		t.Fatalf("last timeline event = %+v, want a delivered event labeled fire", last)
	}

	// A THIRD restart, after the redelivery already succeeded, must not redeliver it again --
	// the incident's last event is now "delivered", not "fired".
	incidents3, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var delivered2 []Alert
	engine2 := newFleetAlertEngine(
		func() time.Time { return time.Unix(1200, 0) },
		func(a Alert) bool { mu.Lock(); delivered2 = append(delivered2, a); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents3,
	)
	engine2.waitIdleForTest()
	if len(delivered2) != 0 {
		t.Fatalf("redelivered a second time after already succeeding: %+v", delivered2)
	}
}

// TestEngineResurrectsBothLegsFireBeforeRecover: if the master crashed with BOTH the fire
// and its recover recorded but neither delivered, resurrection must redeliver both.
func TestEngineResurrectsBothLegsFireBeforeRecover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{}
	if _, err := incidents1.Apply(incidentApply{
		src: src, alert: Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	inc0, err := incidents1.Apply(incidentApply{
		src: src, alert: Alert{Key: "fleet:node:x:down", Title: "x is back", Severity: SevCritical, Kind: "recover", Time: 1050},
		firedAt: 1050, now: 1050,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Crash: neither leg's delivery goroutine ever ran.

	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(1100, 0) },
		func(a Alert) bool { mu.Lock(); order = append(order, a.Kind); mu.Unlock(); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents2,
	)
	engine.waitIdleForTest()

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "fire" || got[1] != "recover" {
		t.Fatalf("delivery order = %v, want [fire recover]", got)
	}
	inc, ok := incidents2.Get(inc0.ID)
	if !ok {
		t.Fatal("incident missing")
	}
	fireDelivered, recoverDelivered := legDeliveredStatus(inc)
	if !fireDelivered || !recoverDelivered {
		t.Fatalf("legDeliveredStatus = fire=%v recover=%v, want both true", fireDelivered, recoverDelivered)
	}
}

// TestEngineResurrectsOnlyUndeliveredRecoverLeg: the fire already delivered (labeled "fire:
// ...") before the crash; only the recover is missing its "delivered" event.
func TestEngineResurrectsOnlyUndeliveredRecoverLeg(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{}
	fireInc, err := incidents1.Apply(incidentApply{
		src: src, alert: Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incidents1.AppendEvent(fireInc.ID, core.IncidentEvent{
		TS: 1010, Kind: "delivered", Detail: "fire: sent via the master's dispatcher", Actor: "system",
	}); err != nil {
		t.Fatal(err)
	}
	inc0, err := incidents1.Apply(incidentApply{
		src: src, alert: Alert{Key: "fleet:node:x:down", Title: "x is back", Severity: SevCritical, Kind: "recover", Time: 1050},
		firedAt: 1050, now: 1050,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Crash before the recover's own delivery.

	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(1100, 0) },
		func(a Alert) bool { delivered = append(delivered, a); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents2,
	)
	engine.waitIdleForTest()

	if len(delivered) != 1 || delivered[0].Kind != "recover" {
		t.Fatalf("delivered = %+v, want exactly one recover redelivery", delivered)
	}
	inc, ok := incidents2.Get(inc0.ID)
	if !ok {
		t.Fatal("incident missing")
	}
	fireDelivered, recoverDelivered := legDeliveredStatus(inc)
	if !fireDelivered || !recoverDelivered {
		t.Fatalf("legDeliveredStatus = fire=%v recover=%v, want both true after resurrection", fireDelivered, recoverDelivered)
	}
}

// TestEngineDoesNotResurrectMasterOwnAlertOlderThan24h covers the 24h cutoff: an
// un-delivered master-own fire older than 24h is given up on, not redelivered.
func TestEngineDoesNotResurrectMasterOwnAlertOlderThan24h(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	inc0, err := incidents1.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000},
		firedAt: 1000, now: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}

	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(1000+int64(25*time.Hour/time.Second), 0) }, // > 24h later
		func(a Alert) bool { delivered = append(delivered, a); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents2,
	)
	engine.waitIdleForTest()

	if len(delivered) != 0 {
		t.Fatalf("delivered = %+v, want none (entry older than 24h)", delivered)
	}
	inc, ok := incidents2.Get(inc0.ID)
	if !ok {
		t.Fatal("incident missing")
	}
	last := inc.Timeline[len(inc.Timeline)-1]
	if last.Kind != "suppressed" || last.Detail != "not delivered: master restarted" {
		t.Fatalf("last timeline event = %+v, want the not-delivered-after-restart note", last)
	}
}

func TestEngineTickLeasesCadenceAndOnConnect(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	t0 := time.Unix(2_000_000_000, 0)

	ef.engine.TickLeases(t0, []string{"n1"})
	if got := len(ef.framesFor("n1", "lease")); got != 1 {
		t.Fatalf("leases after first tick = %d, want 1", got)
	}
	// 10s later: cadence is 30s, so no second push yet.
	ef.engine.TickLeases(t0.Add(10*time.Second), []string{"n1"})
	if got := len(ef.framesFor("n1", "lease")); got != 1 {
		t.Fatalf("leases after +10s = %d, want still 1", got)
	}
	// 31s later: due again.
	ef.engine.TickLeases(t0.Add(31*time.Second), []string{"n1"})
	if got := len(ef.framesFor("n1", "lease")); got != 2 {
		t.Fatalf("leases after +31s = %d, want 2", got)
	}
	// A disconnected/removed/revoked node (simulated here by omitting it from the ids list,
	// exactly as masterLoop.tick filters revoked nodes) gets no lease at all.
	ef.engine.TickLeases(t0.Add(62*time.Second), nil)
	if got := len(ef.framesFor("n1", "lease")); got != 2 {
		t.Fatalf("leases after being excluded = %d, want still 2", got)
	}

	// PushLeaseNow (Hub.OnConnect) bypasses the cadence gate entirely.
	ef.disconnect("n1")
	ef.connect("n2")
	ef.engine.PushLeaseNow("n2", t0)
	if got := len(ef.framesFor("n2", "lease")); got != 1 {
		t.Fatalf("PushLeaseNow leases = %d, want 1", got)
	}
}

func TestEngineIncidentsReloadAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	incidents, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	engine := newFleetAlertEngine(func() time.Time { return now }, func(Alert) bool { return true }, func(string, fleet.Frame) bool { return true }, func(string) bool { return true }, incidents)
	engine.PushLeaseNow("n1", now.Add(-time.Minute))
	engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})

	// "Restart": a fresh incidentStore and engine over the same file.
	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	incs := incidents2.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].State != "firing" || incs[0].Alerts[0].Key != "cpu" {
		t.Fatalf("reloaded incidents = %+v", incs)
	}

	var delivered []Alert
	engine2 := newFleetAlertEngine(func() time.Time { return now }, func(a Alert) bool { delivered = append(delivered, a); return true }, func(string, fleet.Frame) bool { return true }, func(string) bool { return true }, incidents2)
	// The restarted engine's dedup set is seeded from the reloaded incidents, so re-processing
	// the exact same (node, key, fired_at) record must not redeliver it.
	engine2.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})
	if len(delivered) != 0 {
		t.Fatalf("delivered after restart replay = %+v, want none", delivered)
	}
}

func TestEnginePushAckAndUnackFrames(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushAck("n1", "cpu", false)
	ef.engine.PushAck("n1", "cpu", true)

	acks := ef.framesFor("n1", "ack")
	unacks := ef.framesFor("n1", "unack")
	if len(acks) != 1 || len(unacks) != 1 {
		t.Fatalf("acks = %d, unacks = %d, want 1 each", len(acks), len(unacks))
	}
	var d ackFrameData
	if err := json.Unmarshal(acks[0].Data, &d); err != nil || d.Key != "cpu" {
		t.Fatalf("ack frame data = %+v err %v", d, err)
	}
}

// TestReplicaSinkOnAlertHookDedupsByteIdenticalRecord covers the wiring from
// replicaSink.apply's KindAlert branch into the engine.
func TestReplicaSinkOnAlertHookDedupsByteIdenticalRecord(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []AlertEvent
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, func(nodeID string, ev AlertEvent) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	})
	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	data, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	rec := fleet.Record{Seq: 1, Kind: fleet.KindAlert, TS: 1000, Data: data}

	if err := sink.Apply(testNodeID, []fleet.Record{rec}); err != nil {
		t.Fatal(err)
	}
	// The exact same record again via Backfill (as gap repair or a retried batch would send
	// it): byte-identical.
	if err := sink.Backfill(testNodeID, []fleet.Record{rec}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("onAlert calls = %d, want 1: %+v", len(got), got)
	}
}

// --- keyed dispatch ordering + bounded concurrency -----

// TestEngineFireThenRecoverDeliveredInOrderEvenOnSlowChannel exercises the keyed dispatcher
// (fleet_dispatch.go) through Submit/HandleChildAlert directly.
func TestEngineFireThenRecoverDeliveredInOrderEvenOnSlowChannel(t *testing.T) {
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(500, 0)
	var mu sync.Mutex
	var order []string
	engine := newFleetAlertEngine(
		func() time.Time { return now },
		func(a Alert) bool {
			if a.Kind == "fire" {
				time.Sleep(50 * time.Millisecond) // the slow channel
			}
			mu.Lock()
			order = append(order, a.Kind)
			mu.Unlock()
			return true
		},
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	engine.PushLeaseNow("n1", now)
	engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: 1000,
	})
	engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1050, Key: "cpu", Title: "cpu back to normal", Severity: "warning", Kind: "recover", Source: "anomaly",
		RoutedToMaster: true, FiredAt: 1050,
	})
	engine.waitIdleForTest()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "fire" || order[1] != "recover" {
		t.Fatalf("delivery order = %v, want [fire recover]", order)
	}
}

// TestEngineBoundsConcurrentDispatches is the engine-level counterpart of
// TestKeyedDispatcherBoundsGlobalConcurrency: 200 concurrent Submits.
func TestEngineBoundsConcurrentDispatches(t *testing.T) {
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(500, 0)

	var mu sync.Mutex
	current, max := 0, 0
	engine := newFleetAlertEngine(
		func() time.Time { return now },
		func(Alert) bool {
			mu.Lock()
			current++
			if current > max {
				max = current
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			current--
			mu.Unlock()
			return true
		},
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nodeID := keyFor(i)
			engine.PushLeaseNow(nodeID, now)
			engine.HandleChildAlert(nodeID, "box", nil, AlertEvent{
				Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
				RoutedToMaster: true, FiredAt: 1000,
			})
		}(i)
	}
	wg.Wait() // every Submit call has returned (it never blocks on dispatch)
	engine.waitIdleForTest()

	mu.Lock()
	defer mu.Unlock()
	if max > dispatchConcurrency {
		t.Fatalf("observed %d concurrent dispatches through the engine, want <= %d", max, dispatchConcurrency)
	}
}

// TestEngineStopLeavesUndeliveredJobCleanForResurrection: a job still in flight when Stop's
// timeout expires leaves the incident as step 1 recorded it (no "delivered" event).
func TestEngineStopLeavesUndeliveredJobCleanForResurrection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")
	incidents, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(500, 0) },
		func(Alert) bool { <-block; return true }, // deliberately stuck
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	engine.Submit(alertSource{}, Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Time: 1000})

	if ok := engine.Stop(50 * time.Millisecond); ok {
		t.Fatal("Stop reported everything drained, but the job is deliberately stuck")
	}

	// While the job is STILL stuck (block not yet closed): the durable record from step 1
	// shows no "delivered" event.
	inc, ok := incidents.FindByAlertKey("fleet:node:x:down")
	if !ok {
		t.Fatal("incident missing")
	}
	if inc.State != "firing" {
		t.Fatalf("incident state = %q, want firing", inc.State)
	}
	if fireDelivered, _ := legDeliveredStatus(inc); fireDelivered {
		t.Fatal("fire leg shows delivered, but the job is deliberately still stuck")
	}

	// A fresh engine over the same store resurrects it while the original is stuck.
	var delivered []Alert
	engine2 := newFleetAlertEngine(
		func() time.Time { return time.Unix(600, 0) },
		func(a Alert) bool { delivered = append(delivered, a); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		incidents,
	)
	engine2.waitIdleForTest()
	if len(delivered) != 1 || delivered[0].Key != "fleet:node:x:down" {
		t.Fatalf("delivered after restart = %+v, want exactly one redelivery", delivered)
	}

	// Clean up: let the original stuck job finish so nothing leaks past this test.
	close(block)
	engine.waitIdleForTest()
}

func TestEngineHandleChildAckSyncAcksOpenIncident(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})
	ef.waitIdle()

	as, _ := json.Marshal(AlertState{Active: map[string]ActiveAlert{"cpu": {Since: 1000, Acked: true, AckedAt: 1010}}})
	ef.engine.HandleChildAckSync("n1", as)

	inc, ok := ef.incidents.OpenAlertIncident("n1", "cpu")
	if !ok || inc.State != "acked" || inc.AckedBy != "node:n1" {
		t.Fatalf("incident after ack sync = %+v ok=%v", inc, ok)
	}
}

// --- ordering (record -> deliver -> receipt) ----------

// TestEngineReceiptFollowsDeliveredEventOnSuccess: the receipt goes out only AFTER the
// "delivered" timeline event is durably recorded.
func TestEngineReceiptFollowsDeliveredEventOnSuccess(t *testing.T) {
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(500, 0)

	var mu sync.Mutex
	var receiptPushed, sawDeliveredBeforeReceipt bool
	engine := newFleetAlertEngine(
		func() time.Time { return now },
		func(Alert) bool { return true }, // every delivery succeeds
		func(node string, f fleet.Frame) bool {
			if f.Type == "receipt" {
				mu.Lock()
				receiptPushed = true
				if inc, ok := incidents.FindByAlertKey("cpu"); ok {
					for _, e := range inc.Timeline {
						if e.Kind == "delivered" {
							sawDeliveredBeforeReceipt = true
						}
					}
				}
				mu.Unlock()
			}
			return true
		},
		func(string) bool { return true },
		incidents,
	)
	engine.PushLeaseNow("n1", now)
	engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: 1000,
	})
	engine.waitIdleForTest()

	mu.Lock()
	defer mu.Unlock()
	if !receiptPushed {
		t.Fatal("receipt was never pushed")
	}
	if !sawDeliveredBeforeReceipt {
		t.Fatal("receipt was pushed before the delivered event was recorded")
	}
}

// TestEngineAllChannelsFailSendsNoReceipt: when all channels fail, no receipt goes out and
// the incident stays firing with no "delivered" event.
func TestEngineAllChannelsFailSendsNoReceipt(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.deliverFail = true

	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: 1000,
	})
	ef.waitIdle()

	if ef.deliveredCount() != 0 {
		t.Fatalf("delivered = %+v, want none recorded (every channel failed)", ef.delivered)
	}
	if got := len(ef.framesFor("n1", "receipt")); got != 0 {
		t.Fatalf("receipts = %d, want 0", got)
	}
	inc, ok := ef.incidents.FindByAlertKey("cpu")
	if !ok {
		t.Fatal("incident missing")
	}
	if inc.State != "firing" {
		t.Fatalf("incident state = %q, want still firing", inc.State)
	}
	for _, e := range inc.Timeline {
		if e.Kind == "delivered" {
			t.Fatalf("timeline should not show delivered when every channel failed: %+v", inc.Timeline)
		}
	}
}

// TestEngineCrashBetweenRecordAndReceiptThenChildFallback: incidents.Apply records the fire
// durably, then the process dies before delivery (no receipt sent).
func TestEngineCrashBetweenRecordAndReceiptThenChildFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	// Step 1 only, exactly what Submit does before ever attempting delivery -- simulating a
	// crash immediately after this, before any goroutine for delivery ever ran.
	incidents1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incidents1.Apply(incidentApply{
		src:              alertSource{NodeID: "n1", NodeName: "box1"},
		alert:            Alert{Key: "cpu", Title: "cpu high", Severity: SevWarning, Kind: "fire", Time: 1000},
		firedAt:          1000,
		deliveredLocally: false,
		now:              1000,
	}); err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh incidentStore/engine over the same file.
	incidents2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var delivered []Alert
	var pushed []fleet.Frame
	engine2 := newFleetAlertEngine(
		func() time.Time { return time.Unix(2000, 0) },
		func(a Alert) bool { delivered = append(delivered, a); return true },
		func(node string, f fleet.Frame) bool { pushed = append(pushed, f); return true },
		func(string) bool { return true },
		incidents2,
	)

	// The child's fallback: same (node, key, fired_at), DeliveredLocally true, RoutedToMaster
	// left at its zero value (deliverFallback never sets it).
	engine2.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1300, Key: "cpu", Title: "via local fallback: master unreachable — cpu high",
		Severity: "warning", Kind: "fire", Source: "anomaly", DeliveredLocally: true, FiredAt: 1000,
	})
	engine2.waitIdleForTest()

	if len(delivered) != 0 {
		t.Fatalf("engine redelivered after the child's fallback: %+v", delivered)
	}
	if len(pushed) != 0 {
		t.Fatalf("engine pushed a frame after the child's fallback: %+v", pushed)
	}
	inc, ok := incidents2.FindByAlertKey("cpu")
	if !ok {
		t.Fatal("incident missing")
	}
	if len(inc.Alerts) != 1 || !inc.Alerts[0].DeliveredLocally {
		t.Fatalf("incident alert not marked delivered_locally: %+v", inc.Alerts)
	}
	foundChildDelivered := false
	for _, e := range inc.Timeline {
		if e.Kind == "delivered" && e.Actor == "child" {
			foundChildDelivered = true
		}
	}
	if !foundChildDelivered {
		t.Fatalf("timeline missing the child-delivered event: %+v", inc.Timeline)
	}
}

// --- rotation + restart dedup memory ------------------

// TestEngineDedupSurvivesRotationAndReplay covers: an incident resolves, incidents.jsonl
// rotates (the resolved incident's only record of it moves to incidents.jsonl.1).
func TestEngineDedupSurvivesRotationAndReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "incidents.jsonl")

	s1, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	src := alertSource{NodeID: "n1", NodeName: "box1"}
	if _, err := s1.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "fire", Severity: SevWarning, Time: 1000}, firedAt: 1000, now: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Apply(incidentApply{src: src, alert: Alert{Key: "cpu", Kind: "recover", Severity: SevWarning, Time: 1050}, firedAt: 1050, now: 1050}); err != nil {
		t.Fatal(err)
	}

	// Force rotation: pad the file past the threshold with harmless newline-delimited filler
	// (long lines, not one giant token -- see TestIncidentStoreRotatesAt50MB).
	line := append(bytes.Repeat([]byte("x"), 4096), '\n')
	pad := bytes.Repeat(line, incidentRotateBytes/len(line)+1)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(pad); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Apply(incidentApply{src: alertSource{NodeID: "n2"}, alert: Alert{Key: "mem", Kind: "fire", Severity: SevWarning, Time: 2000}, firedAt: 2000, now: 2000}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotation to %s.1: %v", path, err)
	}

	// "Restart": a fresh store + engine over the (now rotated) file.
	s2, err := loadIncidentStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var delivered []Alert
	engine := newFleetAlertEngine(
		func() time.Time { return time.Unix(3000, 0) },
		func(a Alert) bool { delivered = append(delivered, a); return true },
		func(string, fleet.Frame) bool { return true },
		func(string) bool { return true },
		s2,
	)

	// Replay the ORIGINAL (now rotated-away) fire record.
	engine.HandleChildAlert("n1", "box1", nil, AlertEvent{
		Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly",
		RoutedToMaster: true, FiredAt: 1000,
	})
	engine.waitIdleForTest()

	if len(delivered) != 0 {
		t.Fatalf("redelivered a rotated-away record after restart: %+v", delivered)
	}
	for _, inc := range s2.List(core.IncidentFilter{}, nil) {
		if inc.GroupKey == "n1:cpu" {
			t.Fatalf("orphan incident recreated for n1/cpu after rotation+restart: %+v", inc)
		}
	}
}
