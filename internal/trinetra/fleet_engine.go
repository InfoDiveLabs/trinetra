// Package trinetra: fleet_engine.go is the master's alerting engine: the
// single place a child's shipped alert (via replicaSink's OnAlert hook) and
// the master's own fleet alerts (node-down, masterLoop.fleetAlert) both
// funnel through on their way to delivery. It enriches, dedups, decides
// whether the alert still needs delivering (a child that already delivered
// it locally must never be redelivered), folds it into an incident, and --
// for a routed alert -- pushes a receipt back down the node's stream.
//
// Routing/silences/dependency/grouping/escalation (the rest of spec
// section 6's pipeline) arrive in later tasks; this is the core the rest of
// that pipeline is built on.
package trinetra

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// leaseInterval/leaseValidFor are the lease cadence (global-constraints:
// sent every 30s, valid 90s).
const (
	leaseInterval = 30 * time.Second
	leaseValidFor = 90 * time.Second
)

// alertSource identifies who an alert reaching the engine is about: a fleet
// node (NodeID set, enriched with its name/tags for the delivered title and
// future routing) or the master's own synthetic alerts (NodeID "").
type alertSource struct {
	NodeID   string
	NodeName string
	Tags     []string
	// DeliveredLocally is true when the record itself says the child already
	// delivered this alert locally (the fallback record, or -- see
	// hadLeaseBefore -- a node that never held a lease at all): the engine
	// must record it but never call deliver for it.
	DeliveredLocally bool
}

// alertDedupKey is the master's dedup key for one alert record:
// (node_id, alert_key, fired_at), per global-constraints. node is "" for the
// master's own alerts.
type alertDedupKey struct {
	node    string
	key     string
	firedAt int64
}

// fleetAlertEngine is the master's alerting engine core (spec 6): enrich ->
// dedup -> decide local-vs-master delivery -> record -> deliver -> receipt.
//
// deliver is synchronous and reports whether at least one channel actually
// accepted the alert (see Submit's doc comment for why the ORDER of
// record/deliver/receipt matters): the master's own wiring
// (fleetDeps.alert, daemon.go) is deliverSyncAndLog, which calls the
// existing Dispatcher directly rather than going through the async
// NotifierQueue -- the queue's own drop policy exists for the high-volume
// local anomaly path, not this one, and reporting completion is the whole
// point here.
type fleetAlertEngine struct {
	now       func() time.Time
	deliver   func(Alert) bool
	push      func(nodeID string, f fleet.Frame) bool
	connected func(nodeID string) bool
	incidents *incidentStore

	mu   sync.Mutex
	seen map[alertDedupKey]struct{}

	leaseMu       sync.Mutex
	lastLeasePush map[string]int64 // node id -> unix time of the last push attempt
	firstLeaseAt  map[string]int64 // node id -> unix time of the first ever SUCCESSFUL push

	// sendResolved gates whether a recover is actually delivered (still
	// always recorded in the incident either way). Hard-coded true for now:
	// per-route policy (alerting.json) arrives in a later task.
	sendResolved bool

	// wg tracks in-flight deliverAndReceipt goroutines (see Submit), purely
	// so a test can deterministically wait for one to finish
	// (waitIdleForTest) instead of racing it.
	wg sync.WaitGroup
}

// newFleetAlertEngine builds a fleetAlertEngine. now defaults to time.Now if
// nil. The dedup set is seeded from incidents' own history (current file AND
// its previous rotation, see incidentStore.seenKeys), so a restarted master
// never redelivers an alert it already processed before restarting, even
// across a rotation.
func newFleetAlertEngine(now func() time.Time, deliver func(Alert) bool, push func(string, fleet.Frame) bool, connected func(string) bool, incidents *incidentStore) *fleetAlertEngine {
	if now == nil {
		now = time.Now
	}
	seen := map[alertDedupKey]struct{}{}
	if incidents != nil {
		seen = incidents.seenKeys()
	}
	return &fleetAlertEngine{
		now: now, deliver: deliver, push: push, connected: connected,
		incidents: incidents, seen: seen,
		lastLeasePush: map[string]int64{}, firstLeaseAt: map[string]int64{},
		sendResolved: true,
	}
}

// Submit is the engine's single entry point: every child-shipped alert (via
// HandleChildAlert) and every master-generated alert (masterLoop's own
// fleetAlert, src is the zero alertSource) reaches delivery/incidents only
// through here.
//
// Ordering (B3 review round 1 fix): the decision is durably recorded via
// incidents.Apply BEFORE any delivery is attempted, so a crash between
// recording and a receipt going out is always recoverable from disk -- the
// incident already shows "fired". A receipt is pushed to the child ONLY
// after delivery has actually completed with at least one channel
// succeeding (never before, and never for an alert that already delivered
// locally). If every channel fails (or none matched), no receipt is sent:
// the child's own fallback then fires on schedule, producing a SECOND
// record for the exact same (node, key, fired_at) with
// DeliveredLocally=true -- Submit's dedup keys off precisely that tuple, so
// this second record is recognized as "the same alert, now known to have
// been delivered locally" (see the alreadySeen branch below) rather than
// either silently dropped (global-constraints: a delivered_locally record
// must be RECORDED) or redelivered (it must never trigger a second delivery
// attempt).
//
// Delivery (steps 2-4) runs on its own goroutine so Submit itself never
// blocks its caller (replicaNode.apply holds n.mu across this call, and
// masterLoop.tick calls it inline) on the Dispatcher's ~15s-per-channel
// timeout.
func (e *fleetAlertEngine) Submit(src alertSource, a Alert) {
	firedAt := a.Time
	key := alertDedupKey{node: src.NodeID, key: a.Key, firedAt: firedAt}
	e.mu.Lock()
	_, alreadySeen := e.seen[key]
	e.seen[key] = struct{}{}
	e.mu.Unlock()

	hadLease := true
	if src.NodeID != "" {
		hadLease = e.hadLeaseBefore(src.NodeID, firedAt)
	}
	deliveredLocally := src.DeliveredLocally || !hadLease

	if alreadySeen {
		// We already made the fire/recover decision for this exact (node,
		// key, fired_at) once. Most of the time this is a byte-identical
		// resend (backfill + ingest both delivering the same record) and
		// there is nothing left to do. The one case worth a further durable
		// update is a LATER record for the same key that newly reveals
		// deliveredLocally -- e.g. this master crashed (or a receipt was
		// simply lost) between recording the original fire and its receipt
		// reaching the child, the child's fallback then delivered it
		// locally on its own, and that fallback record is what just
		// arrived here. That must be recorded (never silently dropped) but
		// must NEVER trigger a delivery attempt: this Submit call already
		// decided that, the first time it saw this key.
		if deliveredLocally && e.incidents != nil {
			_, _, _ = e.incidents.MarkDeliveredLocally(src.NodeID, a.Key, firedAt, e.now().Unix())
		}
		return
	}

	// Step 1: durably record the decision before attempting delivery.
	var incidentID string
	if e.incidents != nil {
		inc, err := e.incidents.Apply(incidentApply{
			src: src, alert: a, firedAt: firedAt,
			deliveredLocally: deliveredLocally, now: e.now().Unix(),
		})
		if err == nil {
			incidentID = inc.ID
		}
	}

	if deliveredLocally {
		return // the child already delivered this; nothing more to do.
	}

	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.deliverAndReceipt(src, a, firedAt, incidentID)
	}()
}

// deliverAndReceipt is Submit's steps 2-4, run off Submit's own goroutine:
// attempt delivery, and ONLY on success (at least one channel accepted it)
// record the "delivered" timeline event and push the receipt. incidentID
// may be "" if step 1's Apply failed to record anything -- delivery still
// proceeds (the alert must still reach its channels), but there is nothing
// to append a "delivered" event to.
func (e *fleetAlertEngine) deliverAndReceipt(src alertSource, a Alert, firedAt int64, incidentID string) {
	if a.Kind == "recover" && !e.sendResolved {
		return
	}
	if e.deliver == nil {
		return
	}
	da := a
	if src.NodeID != "" {
		da.Title = src.NodeName + ": " + a.Title
	}
	if !e.deliver(da) {
		return // no channel accepted it: no receipt, no "delivered" event.
	}
	if e.incidents != nil && incidentID != "" {
		_, _ = e.incidents.AppendEvent(incidentID, core.IncidentEvent{
			TS: e.now().Unix(), Kind: "delivered", Detail: "sent via the master's dispatcher", Actor: "system",
		})
	}
	if src.NodeID != "" && e.push != nil {
		e.pushReceipt(src.NodeID, a.Key, firedAt)
	}
}

// waitIdleForTest blocks until every in-flight deliverAndReceipt goroutine
// Submit has spawned has finished. Test-only: production code never needs
// Submit's delivery to be synchronous from the caller's point of view.
func (e *fleetAlertEngine) waitIdleForTest() { e.wg.Wait() }

// HandleChildAlert converts a child's shipped AlertEvent (as decoded by the
// replica from a KindAlert record) into an Alert and submits it, using
// FiredAt (falling back to Time for an event logged before that field
// existed) as the alert's own time throughout -- dedup, delivery and the
// incident all key off when the alert actually fired, not when a fallback
// record for it happened to be appended.
//
// The record itself is treated as already delivered locally by the child
// whenever ev.DeliveredLocally is set (the fallback record) OR
// ev.RoutedToMaster is NOT set: RoutedToMaster false means the child's own
// Route() decided, at fire time, that it held no valid lease and delivered
// locally right then (see fleet_lease.go's handoff.Route) -- there is no
// third case. Submit applies a further, engine-side override on top of this
// (hadLeaseBefore, Review Focus 5): even a record claiming RoutedToMaster
// true is still treated as delivered locally if THIS master's own
// bookkeeping shows it never actually got a lease to this node before
// firedAt, which can happen if the child's own lease validity outlived a
// master restart that wiped the new process's lease history.
func (e *fleetAlertEngine) HandleChildAlert(nodeID, nodeName string, tags []string, ev AlertEvent) {
	sev, err := ParseSeverity(ev.Severity)
	if err != nil {
		sev = SevWarning
	}
	firedAt := ev.FiredAt
	if firedAt == 0 {
		firedAt = ev.Time
	}
	a := Alert{Key: ev.Key, Title: ev.Title, Severity: sev, Kind: ev.Kind, Source: ev.Source, Time: firedAt}
	deliveredLocally := ev.DeliveredLocally || !ev.RoutedToMaster
	e.Submit(alertSource{NodeID: nodeID, NodeName: nodeName, Tags: tags, DeliveredLocally: deliveredLocally}, a)
}

// hadLeaseBefore reports whether nodeID had ever been successfully pushed a
// lease at or before firedAt (Review Focus 5: a node that never held a
// lease -- e.g. it was not connected to the stream at fire time -- cannot
// have routed this alert to the master, so it must be treated as already
// delivered locally, never redelivered).
func (e *fleetAlertEngine) hadLeaseBefore(nodeID string, firedAt int64) bool {
	e.leaseMu.Lock()
	defer e.leaseMu.Unlock()
	first, ok := e.firstLeaseAt[nodeID]
	return ok && first <= firedAt
}

func (e *fleetAlertEngine) pushReceipt(nodeID, key string, firedAt int64) {
	data, err := json.Marshal(receiptFrameData{Key: key, FiredAt: firedAt})
	if err != nil {
		return
	}
	e.push(nodeID, fleet.Frame{Type: "receipt", Data: data})
}

// TickLeases pushes a fresh lease to every connected node in ids whose last
// lease push was leaseInterval or more ago (or never pushed). Revoked or
// removed nodes must simply not be in ids -- the caller (masterLoop.tick)
// owns that filtering against the registry.
func (e *fleetAlertEngine) TickLeases(now time.Time, ids []string) {
	for _, id := range ids {
		if e.connected != nil && !e.connected(id) {
			continue
		}
		e.maybePushLease(id, now)
	}
}

func (e *fleetAlertEngine) maybePushLease(id string, now time.Time) {
	e.leaseMu.Lock()
	last, ok := e.lastLeasePush[id]
	due := !ok || now.Unix()-last >= int64(leaseInterval/time.Second)
	if due {
		e.lastLeasePush[id] = now.Unix()
	}
	e.leaseMu.Unlock()
	if due {
		e.pushLease(id, now)
	}
}

// PushLeaseNow pushes a lease to id unconditionally, bypassing the 30s
// cadence gate -- used on Hub.OnConnect, so a freshly (re)connected node
// gets a lease immediately rather than waiting up to one masterTickInterval.
func (e *fleetAlertEngine) PushLeaseNow(id string, now time.Time) {
	e.leaseMu.Lock()
	e.lastLeasePush[id] = now.Unix()
	e.leaseMu.Unlock()
	e.pushLease(id, now)
}

func (e *fleetAlertEngine) pushLease(id string, now time.Time) {
	data, err := json.Marshal(leaseFrameData{Until: now.Add(leaseValidFor).Unix()})
	if err != nil {
		return
	}
	if e.push == nil {
		return
	}
	if ok := e.push(id, fleet.Frame{Type: "lease", Data: data}); ok {
		e.leaseMu.Lock()
		if _, seen := e.firstLeaseAt[id]; !seen {
			e.firstLeaseAt[id] = now.Unix()
		}
		e.leaseMu.Unlock()
	}
}

// HandleChildAckSync is the master's entry point for a child-side ack/unack
// reaching it: it is called whenever a node's alerts.json actually changed
// (replicaSink.Live -> onAckSync), the only channel a child's own
// AlertState.Ack/Unack -- whether from a manual `trinetra alerts ack` on
// that node, or the master's own AckIncident push applied there -- reaches
// the master through. Every currently-acked active alert acks the incident
// it belongs to, if that incident is still open and not already acked.
func (e *fleetAlertEngine) HandleChildAckSync(nodeID string, as json.RawMessage) {
	if e.incidents == nil {
		return
	}
	var state AlertState
	if json.Unmarshal(as, &state) != nil {
		return
	}
	now := e.now().Unix()
	for key, active := range state.Active {
		if !active.Acked {
			continue
		}
		inc, ok := e.incidents.OpenForGroupKey(incidentGroupKey(nodeID, key))
		if !ok || inc.State == "acked" || inc.State == "resolved" {
			continue
		}
		_, _ = e.incidents.Ack(inc.ID, "node:"+nodeID, now)
	}
}

// PushAck pushes an "ack" (unack=false) or "unack" (unack=true) frame for
// key to nodeID, applied on the child via AlertState.Ack/Unack (see
// applyAckFrame in fleet_lease.go). Failure (node not connected) is silent:
// the child's own state is what AckIncident is really updating on the
// master side; the frame is best-effort propagation.
func (e *fleetAlertEngine) PushAck(nodeID, key string, unack bool) {
	if e.push == nil {
		return
	}
	data, err := json.Marshal(ackFrameData{Key: key})
	if err != nil {
		return
	}
	typ := "ack"
	if unack {
		typ = "unack"
	}
	e.push(nodeID, fleet.Frame{Type: typ, Data: data})
}
