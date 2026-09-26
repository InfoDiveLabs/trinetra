package trinetra

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// engineFixture wires a fleetAlertEngine over a real incidentStore/auditLog
// (both on disk, like production) with fake push/deliver/connected so a test
// can assert exactly what the engine decided without any network or
// dispatcher machinery.
type engineFixture struct {
	mu        sync.Mutex
	delivered []Alert
	pushed    []pushedFrame
	connected map[string]bool

	incidents *incidentStore
	audit     *auditLog
	engine    *fleetAlertEngine
	now       time.Time
}

type pushedFrame struct {
	node string
	f    fleet.Frame
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	dir := t.TempDir()
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ef := &engineFixture{
		connected: map[string]bool{},
		incidents: incidents,
		audit:     newAuditLog(filepath.Join(dir, "audit.jsonl")),
		now:       time.Unix(1_700_000_000, 0),
	}
	ef.engine = newFleetAlertEngine(
		func() time.Time { return ef.now },
		func(a Alert) { ef.mu.Lock(); ef.delivered = append(ef.delivered, a); ef.mu.Unlock() },
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
		incidents, ef.audit,
	)
	return ef
}

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

func TestEngineDedupsSameAlertArrivingTwice(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))

	ev := AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000}
	// Same (node, key, fired_at) arriving twice -- e.g. once via Backfill,
	// once via Ingest -- must be processed (delivered, receipted) only once.
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)
	ef.engine.HandleChildAlert("n1", "box1", nil, ev)

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

	// The fallback record: RoutedToMaster stays false (deliverFallback never
	// sets it), DeliveredLocally true, FiredAt carries the ORIGINAL fire's
	// time (see deliverFallback's doc comment).
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

	// RoutedToMaster false (the child had no valid lease at fire time and
	// delivered locally then and there) but DeliveredLocally is the
	// zero-value false too -- there is no third case (see HandleChildAlert's
	// doc comment): this must still be treated as already delivered.
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
	// n1 is connected, but never got a SUCCESSFUL lease push (no PushLeaseNow
	// call, no TickLeases pass touched it): even though this record claims
	// RoutedToMaster true, Review Focus 5 says the master must not trust
	// that -- it never actually gave this node a lease, so the alert must be
	// treated as already delivered locally.
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
	// The node's FIRST successful lease happens AFTER this alert's fired_at:
	// from the master's perspective it had not yet given this node a lease
	// when the alert fired.
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

	recov := AlertEvent{Time: 1050, Key: "cpu", Title: "cpu back to normal", Severity: "warning", Kind: "recover", Source: "anomaly", RoutedToMaster: true, FiredAt: 1050}
	ef.engine.HandleChildAlert("n1", "box1", nil, recov)

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

	if ef.deliveredCount() != 1 {
		t.Fatalf("delivered count = %d, want 1", ef.deliveredCount())
	}
	if got := ef.lastDelivered().Title; got != "x is down" {
		t.Fatalf("title = %q, want unprefixed %q", got, "x is down")
	}
	incs := ef.incidents.List(core.IncidentFilter{}, nil)
	if len(incs) != 1 || incs[0].GroupKey != "self:fleet:node:x:down" || len(incs[0].Nodes) != 0 {
		t.Fatalf("incident = %+v", incs)
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
	// A disconnected/removed/revoked node (simulated here by omitting it from
	// the ids list, exactly as masterLoop.tick filters revoked nodes) gets no
	// lease at all.
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
	engine := newFleetAlertEngine(func() time.Time { return now }, func(Alert) {}, func(string, fleet.Frame) bool { return true }, func(string) bool { return true }, incidents, nil)
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
	engine2 := newFleetAlertEngine(func() time.Time { return now }, func(a Alert) { delivered = append(delivered, a) }, func(string, fleet.Frame) bool { return true }, func(string) bool { return true }, incidents2, nil)
	// The restarted engine's dedup set is seeded from the reloaded
	// incidents, so re-processing the exact same (node, key, fired_at)
	// record must not redeliver it.
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
// replicaSink.apply's KindAlert branch into the engine: the SAME alert
// record arriving via Backfill (unsequenced, gap repair) and again via
// Ingest -- byte-identical -- must reach onAlert exactly once, per the
// replica's own existing (Time, line) dedup.
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
	// The exact same record again via Backfill (as gap repair or a retried
	// batch would send it): byte-identical, so the replica's own dedup must
	// swallow it before onAlert ever sees it a second time.
	if err := sink.Backfill(testNodeID, []fleet.Record{rec}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("onAlert calls = %d, want 1: %+v", len(got), got)
	}
}

func TestEngineHandleChildAckSyncAcksOpenIncident(t *testing.T) {
	ef := newEngineFixture(t)
	ef.connect("n1")
	ef.engine.PushLeaseNow("n1", time.Unix(500, 0))
	ef.engine.HandleChildAlert("n1", "box1", nil, AlertEvent{Time: 1000, Key: "cpu", Title: "cpu high", Severity: "warning", Kind: "fire", Source: "anomaly", RoutedToMaster: true, FiredAt: 1000})

	as, _ := json.Marshal(AlertState{Active: map[string]ActiveAlert{"cpu": {Since: 1000, Acked: true, AckedAt: 1010}}})
	ef.engine.HandleChildAckSync("n1", as)

	inc, ok := ef.incidents.OpenForGroupKey(incidentGroupKey("n1", "cpu"))
	if !ok || inc.State != "acked" || inc.AckedBy != "node:n1" {
		t.Fatalf("incident after ack sync = %+v ok=%v", inc, ok)
	}
}
