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
	"fmt"
	"strings"
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

	// dispatch runs every deliverAndReceipt call off Submit's own goroutine
	// (see Submit), keyed per (node, key) so a recover is never dispatched
	// before its own fire, and globally bounded to dispatchConcurrency
	// concurrent dispatches (B3 review round 2).
	dispatch *keyedDispatcher

	// silences and nodeInfo are wired once, after construction, via
	// SetSilences (task 4): a nil silences (the default) makes every
	// suppression check and TickSilences call a no-op, so every existing
	// call site/test that never calls SetSilences keeps behaving exactly as
	// before silences existed.
	silences        *silenceStore
	nodeInfo        func(nodeID string) (name string, tags []string)
	lastSilencePush int64 // unix time of the last push-to-all pass; 0 means never

	// alerting, deliverNamed and dispatchOnly (task 5) are wired once, after
	// construction, via SetRouting -- exactly SetSilences's pattern (see its
	// doc comment): a nil alerting keeps every existing call site/test that
	// never calls SetRouting behaving exactly as before routing existed (a
	// single e.deliver call per leg, no escalation, sendResolved hard-coded
	// true above). deliverNamed logs to the alert log/live bus AND dispatches
	// (fire/recover legs, the same role e.deliver plays when alerting is
	// nil); dispatchOnly only dispatches, no logging (escalation/repeat
	// notifications are not new alert records).
	alerting     *alertingStore
	deliverNamed func(a Alert, channels []string) bool
	dispatchOnly func(a Alert, channels []string) bool
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
	e := &fleetAlertEngine{
		now: now, deliver: deliver, push: push, connected: connected,
		incidents: incidents, seen: seen,
		lastLeasePush: map[string]int64{}, firstLeaseAt: map[string]int64{},
		sendResolved: true,
		dispatch:     newKeyedDispatcher(),
	}
	e.resurrectMasterAlerts()
	return e
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
// Delivery (steps 2-4) runs off Submit's own goroutine, through the keyed
// dispatcher (fleet_dispatch.go, B3 review round 2): Enqueue itself never
// blocks Submit's caller (replicaNode.apply holds n.mu across this call, and
// masterLoop.tick calls it inline), the (node, key) lane keeps a fire and
// its later recover in order regardless of how long the fire's own dispatch
// takes, and the dispatcher's global semaphore bounds how many dispatches
// (each up to the Dispatcher's ~15s-per-channel timeout) run at once across
// every key.
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

	// A silence/maintenance window check only matters when this alert would
	// otherwise actually be delivered by the master: a record the child
	// already delivered locally has nothing left to suppress. Matcher.Node
	// matches src.NodeID exactly, or globs src.NodeName -- see core.Matcher's
	// doc comment.
	var supp *suppressionInfo
	if !deliveredLocally && e.silences != nil {
		supp = e.silences.Suppressed(e.now().Unix(), src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())
	}

	// Step 1: durably record the decision before attempting delivery.
	var incidentID string
	if e.incidents != nil {
		inc, err := e.incidents.Apply(incidentApply{
			src: src, alert: a, firedAt: firedAt,
			deliveredLocally: deliveredLocally, suppressed: supp, now: e.now().Unix(),
		})
		if err == nil {
			incidentID = inc.ID
		}
	}

	if deliveredLocally {
		return // the child already delivered this; nothing more to do.
	}

	if supp != nil {
		// Suppressed: recorded above, never dispatched. A child-sourced
		// alert still gets its receipt, so it does not fall back and
		// deliver the silenced alert locally instead.
		if src.NodeID != "" && e.push != nil {
			e.pushReceipt(src.NodeID, a.Key, firedAt)
		}
		return
	}

	e.dispatch.Enqueue(incidentGroupKey(src.NodeID, a.Key), func() {
		e.deliverAndReceipt(src, a, firedAt, incidentID)
	})
}

// deliverAndReceipt is Submit's steps 2-4, run off Submit's own goroutine
// (via the keyed dispatcher): attempt delivery, and ONLY on success record
// the "delivered" timeline event and push the receipt.
func (e *fleetAlertEngine) deliverAndReceipt(src alertSource, a Alert, firedAt int64, incidentID string) {
	e.deliverAndReceiptDetail(src, a, firedAt, incidentID, "sent via the master's dispatcher")
}

// legLabel is "fire" or "recover", exactly matching Alert.Kind for the two
// values fleetAlertEngine ever hands to deliverAndReceiptDetail.
func legLabel(a Alert) string {
	if a.Kind == "recover" {
		return "recover"
	}
	return "fire"
}

// deliverAndReceiptDetail is deliverAndReceipt with a caller-chosen note on
// the "delivered" timeline event -- resurrectMasterAlerts uses this to mark
// a post-restart redelivery distinctly ("redelivered after restart") from an
// ordinary first delivery ("sent via the master's dispatcher"). The event's
// Detail is always prefixed with WHICH LEG it covers ("fire: " or
// "recover: ", B3 review round 3): a master-own incident's fire and recover
// share one IncidentAlert entry, so without this label there would be no way
// to tell, from the incident alone, whether an undelivered leg is the fire,
// the recover, or (before this existed) either -- see legDeliveredStatus,
// which resurrectMasterAlerts uses to tell them apart on restart.
//
// incidentID may be "" if step 1's Apply (or, for a resurrection, the
// incident itself) failed to record anything -- delivery still proceeds
// (the alert must still reach its channels), but there is nothing to append
// a "delivered" event to.
func (e *fleetAlertEngine) deliverAndReceiptDetail(src alertSource, a Alert, firedAt int64, incidentID, note string) {
	da := a
	if src.NodeID != "" {
		da.Title = src.NodeName + ": " + a.Title
	}

	var ok bool
	detail := legLabel(a) + ": " + note
	if e.alerting != nil {
		// Routing/escalation (task 5): resolve the SAME way RouteTest does
		// (resolveRoute), and deliver only to the resolved channels rather
		// than every enabled one.
		cfg := e.alerting.Get()
		res := resolveRoute(cfg, src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())
		var channels []string
		if a.Kind == "recover" {
			if !res.SendResolved {
				return
			}
			// The resolved message goes to the union of every channel that
			// received any step of the fire leg -- an escalation may have
			// widened delivery past step 0's own channels.
			channels = e.unionDeliveredChannels(incidentID)
			if len(channels) == 0 {
				channels = firstStepChannels(res.Steps)
			}
		} else {
			channels = firstStepChannels(res.Steps)
		}
		if e.deliverNamed == nil {
			return
		}
		ok = e.deliverNamed(da, channels)
		detail = legLabel(a) + ": " + stepDetail(0, channels)
	} else {
		if a.Kind == "recover" && !e.sendResolved {
			return
		}
		if e.deliver == nil {
			return
		}
		ok = e.deliver(da)
	}
	if !ok {
		return // no channel accepted it: no receipt, no "delivered" event.
	}
	if e.incidents != nil && incidentID != "" {
		_, _ = e.incidents.AppendEvent(incidentID, core.IncidentEvent{
			TS: e.now().Unix(), Kind: "delivered", Detail: detail, Actor: "system",
		})
	}
	if src.NodeID != "" && e.push != nil {
		e.pushReceipt(src.NodeID, a.Key, firedAt)
	}
}

// stepDetail formats a step-N delivery/escalation/repeat timeline event's
// Detail: "step N: chan1, chan2" (task-5 ruling's literal example for
// "escalated") -- used for every routed delivery (fire's own step 0,
// escalated, repeated) so unionDeliveredChannels/lastStepEventTS can parse
// them all the same way.
func stepDetail(step int, channels []string) string {
	return fmt.Sprintf("step %d: %s", step, strings.Join(channels, ", "))
}

// parseStepChannels extracts the channel list from a stepDetail-formatted
// string (with an optional "fire: "/"recover: " leg prefix already present):
// everything after the LAST ": " separator. Channel names are simple
// identifiers (config.ChannelConfig.Name) that never contain ": ", so this
// is unambiguous.
func parseStepChannels(detail string) []string {
	i := strings.LastIndex(detail, ": ")
	if i < 0 {
		return nil
	}
	var out []string
	for _, c := range strings.Split(detail[i+2:], ", ") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// unionDeliveredChannels returns the deduplicated, order-preserving union of
// every channel that received the FIRE leg's step 0 delivery, any escalation
// or any repeat notification for incidentID -- what a resolved message
// should go to (task-5 ruling), since an escalation may have widened
// delivery past whatever channel(s) the fire itself went to.
func (e *fleetAlertEngine) unionDeliveredChannels(incidentID string) []string {
	if incidentID == "" || e.incidents == nil {
		return nil
	}
	inc, ok := e.incidents.Get(incidentID)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(channels []string) {
		for _, c := range channels {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, ev := range inc.Timeline {
		switch {
		case ev.Kind == "delivered" && strings.HasPrefix(ev.Detail, "fire: "):
			add(parseStepChannels(strings.TrimPrefix(ev.Detail, "fire: ")))
		case ev.Kind == "escalated" || ev.Kind == "repeated":
			add(parseStepChannels(ev.Detail))
		}
	}
	return out
}

// legDeliveredStatus scans inc's timeline for "delivered" events and reports
// which leg(s) (fire, recover) they cover, per deliverAndReceiptDetail's
// "fire: "/"recover: " labeling. A "delivered" event with neither prefix
// predates this labeling (B3 review round 2 and earlier) and, per the
// ruling, counts as fire-delivered -- the only leg that could possibly have
// existed before per-leg tracking was added.
//
// A leg-labelled "suppressed" event (task 4: a silence or maintenance window
// covered this leg) counts as delivered too -- the master decided, on
// purpose, never to deliver it, so resurrectMasterAlerts must not try to
// deliver it now either. An UNLABELLED "suppressed" event (e.g.
// resurrectMasterAlerts's own "not delivered: master restarted", recorded
// once a resurrection attempt itself gives up) explicitly means the
// opposite and must never be treated as delivered, so only the leg-labelled
// form is recognized here.
func legDeliveredStatus(inc core.Incident) (fireDelivered, recoverDelivered bool) {
	for _, ev := range inc.Timeline {
		if ev.Kind != "delivered" && ev.Kind != "suppressed" {
			continue
		}
		switch {
		case strings.HasPrefix(ev.Detail, "fire: "):
			fireDelivered = true
		case strings.HasPrefix(ev.Detail, "recover: "):
			recoverDelivered = true
		case ev.Kind == "delivered":
			fireDelivered = true // pre-leg-labelling "delivered" event
		}
	}
	return fireDelivered, recoverDelivered
}

// resurrectMasterAlerts runs once, at engine construction ("at engine
// start"): master-own alerts (src.NodeID == "", e.g. node-down/
// connectivity) have no child-side fallback if the master crashes between
// recording a fire/recover (Submit's step 1) and actually delivering it --
// unlike a child's own alert, nothing else will ever retry it. It checks the
// FIRE and RECOVER legs of every master-own incident independently (B3
// review round 3: a single check based only on the incident's current
// state/last timeline entry could miss an undelivered fire when the
// recover, recorded later, is the one that happens to look "undelivered"):
// for an incident younger than 24h, any leg with no matching "delivered"
// event is re-enqueued on the SAME keyed lane, fire before recover, so
// ordering is preserved exactly as an ordinary Submit call would produce it.
// 24h or older, it gives up on whichever leg(s) are still undelivered and
// records why, once, without delivering anything.
func (e *fleetAlertEngine) resurrectMasterAlerts() {
	if e.incidents == nil {
		return
	}
	now := e.now()
	cutoff := now.Add(-24 * time.Hour).Unix()
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		if len(inc.Nodes) != 0 || len(inc.Alerts) == 0 {
			continue // not a master-own incident, or nothing recorded on it
		}
		al := inc.Alerts[len(inc.Alerts)-1]
		hasRecover := al.ResolvedAt != 0
		fireDelivered, recoverDelivered := legDeliveredStatus(inc)
		if fireDelivered && (!hasRecover || recoverDelivered) {
			continue // both recorded legs already confirmed delivered
		}

		if inc.Updated < cutoff {
			_, _ = e.incidents.AppendEvent(inc.ID, core.IncidentEvent{
				TS: now.Unix(), Kind: "suppressed", Detail: "not delivered: master restarted", Actor: "system",
			})
			continue
		}

		sev, err := ParseSeverity(al.Severity)
		if err != nil {
			sev = SevWarning
		}
		id := inc.ID
		lane := inc.GroupKey
		if !fireDelivered {
			a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}
			firedAt := al.FiredAt
			e.dispatch.Enqueue(lane, func() {
				e.deliverAndReceiptDetail(alertSource{}, a, firedAt, id, "redelivered after restart")
			})
		}
		if hasRecover && !recoverDelivered {
			a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "recover", Time: al.ResolvedAt}
			firedAt := al.ResolvedAt
			// Enqueued on the SAME lane as the fire above (both use
			// incidentGroupKey via inc.GroupKey), so the keyed dispatcher's
			// per-lane FIFO guarantees the recover never runs first even
			// though both were just enqueued back to back here.
			e.dispatch.Enqueue(lane, func() {
				e.deliverAndReceiptDetail(alertSource{}, a, firedAt, id, "redelivered after restart")
			})
		}
	}
}

// waitIdleForTest blocks until every deliverAndReceipt job Submit has
// enqueued so far has actually run. Test-only: production code never needs
// Submit's delivery to be synchronous from the caller's point of view.
func (e *fleetAlertEngine) waitIdleForTest() { e.dispatch.waitIdleForTest() }

// Stop stops the engine's keyed dispatcher accepting new work and waits up
// to timeout for everything already queued or in flight to finish (see
// keyedDispatcher.Stop's doc comment for what happens to anything left
// undelivered when it expires -- resurrectMasterAlerts at the next start).
func (e *fleetAlertEngine) Stop(timeout time.Duration) bool { return e.dispatch.Stop(timeout) }

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

// SetSilences wires the engine to the master's silence/maintenance store and
// a node info lookup (name, tags), used by Submit's suppression check,
// TickSilences's unsilence-delivery pass and the periodic/on-connect
// "silences" frame push. Called once from startMaster, after both the
// engine and the store exist -- a constructor parameter would force every
// existing (and future) test call site to thread through a store even when
// it never exercises silences at all.
func (e *fleetAlertEngine) SetSilences(store *silenceStore, nodeInfo func(id string) (string, []string)) {
	e.silences = store
	e.nodeInfo = nodeInfo
}

// SetRouting wires the engine to the master's routing/escalation config
// store and its channel-scoped delivery functions (task 5), mirroring
// SetSilences's pattern: called once from startMaster, after the engine
// exists. A nil store (never called, e.g. every pre-existing engine test)
// keeps Submit's delivery, resurrection and unsilenced-redelivery paths
// exactly as they behaved before routing existed -- see
// deliverAndReceiptDetail's alerting==nil branch. deliverNamed is used for
// the fire/recover legs (it also logs to the alert log/live bus, like
// e.deliver); dispatchOnly is used for escalation/repeat notifications
// (dispatch only, no logging -- they are not new alert records).
func (e *fleetAlertEngine) SetRouting(store *alertingStore, deliverNamed, dispatchOnly func(a Alert, channels []string) bool) {
	e.alerting = store
	e.deliverNamed = deliverNamed
	e.dispatchOnly = dispatchOnly
}

// TickSilences prunes long-expired silences, delivers any incident that was
// suppressed but should no longer be (its silence lapsed, or the
// maintenance window ended, while the alert kept firing), and refreshes
// every id's pushed silence set at most once every silencePushInterval (so a
// maintenance window's next-24h expansion stays current even with no
// silence ever changing). ids is the master's current non-revoked node list
// (masterLoop.tick already computes this for TickLeases); a not-actually-
// connected id is simply skipped, exactly like TickLeases.
func (e *fleetAlertEngine) TickSilences(now time.Time, ids []string) {
	if e.silences == nil {
		return
	}
	e.silences.Prune(now.Unix())
	e.deliverUnsilenced(now)

	e.leaseMu.Lock()
	due := e.lastSilencePush == 0 || now.Unix()-e.lastSilencePush >= int64(silencePushInterval/time.Second)
	if due {
		e.lastSilencePush = now.Unix()
	}
	e.leaseMu.Unlock()
	if !due {
		return
	}
	e.PushSilencesToAll(now, ids)
}

// deliverUnsilenced scans the incidents currently indexed as suppressed-and-
// open (incidentStore.SuppressedOpen -- a small, maintained index, not a
// full List() scan every 5s, review round 1, item 3's second half) and, for
// each one whose most recent alert has no ResolvedAt yet, enqueues a
// RE-CHECK job on that incident's own keyed lane. The actual "is it still
// suppressed, still unresolved, still silenced?" decision is made inside
// that job (tryDeliverUnsilenced), not here -- see its doc comment for why.
func (e *fleetAlertEngine) deliverUnsilenced(now time.Time) {
	if e.incidents == nil || e.silences == nil {
		return
	}
	for _, inc := range e.incidents.SuppressedOpen() {
		if len(inc.Alerts) == 0 || inc.Alerts[len(inc.Alerts)-1].ResolvedAt != 0 {
			continue // nothing recorded, or already recovered: nothing to deliver
		}
		id, lane := inc.ID, inc.GroupKey
		e.dispatch.Enqueue(lane, func() { e.tryDeliverUnsilenced(id) })
	}
}

// tryDeliverUnsilenced runs INSIDE id's keyed lane (enqueued by
// deliverUnsilenced above): by the time a lane job actually executes, an
// arbitrary amount of time may have passed and another job for the very
// same incident -- most importantly, its own recover -- may have already
// run ahead of it in that same lane's FIFO order (review round 1, item 3:
// deliverUnsilenced's OLD behaviour decided everything at scan time and
// could deliver a stale "fire" for an incident that had since recovered,
// landing a fire notification AFTER its own recover). So every check that
// matters is re-done here, against the incident's CURRENT state and the
// CURRENT time and silence set, not whatever deliverUnsilenced's scan saw:
// still suppressed, still no recover, and no silence/maintenance window
// matches it any more. Any of those failing is a silent no-op -- there is
// nothing wrong to report; the world just moved on before this job's turn
// came up.
func (e *fleetAlertEngine) tryDeliverUnsilenced(id string) {
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State != "suppressed" || len(inc.Alerts) == 0 {
		return
	}
	al := inc.Alerts[len(inc.Alerts)-1]
	if al.ResolvedAt != 0 {
		return // recovered while this job was queued behind something else
	}
	name, tags := al.Node, []string(nil)
	if al.Node != "" && e.nodeInfo != nil {
		name, tags = e.nodeInfo(al.Node)
	}
	if e.silences.Suppressed(e.now().Unix(), al.Node, name, tags, al.Key, al.Severity) != nil {
		return // silenced again (or still) by the time this job actually ran
	}
	sev, err := ParseSeverity(al.Severity)
	if err != nil {
		sev = SevWarning
	}
	src := alertSource{}
	if al.Node != "" {
		src = alertSource{NodeID: al.Node, NodeName: name, Tags: tags}
	}
	a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}
	e.deliverAndReceiptDetail(src, a, al.FiredAt, id, "delivered after silence ended")
}

// PushSilencesToAll immediately refreshes every id's pushed silence set
// (skipping any not currently connected), bypassing TickSilences's periodic
// cadence gate -- called on any silence/maintenance mutation ("on change").
func (e *fleetAlertEngine) PushSilencesToAll(now time.Time, ids []string) {
	for _, id := range ids {
		if e.connected != nil && !e.connected(id) {
			continue
		}
		e.pushSilencesNow(id, now)
	}
}

// PushSilencesNow pushes id's current filtered silence set unconditionally
// -- used on Hub.OnConnect, mirroring PushLeaseNow, so a freshly
// (re)connected node's fallback path honours the master's current silences
// immediately rather than up to silencePushInterval late.
func (e *fleetAlertEngine) PushSilencesNow(id string, now time.Time) {
	e.pushSilencesNow(id, now)
}

func (e *fleetAlertEngine) pushSilencesNow(id string, now time.Time) {
	if e.push == nil || e.silences == nil {
		return
	}
	name, tags := id, []string(nil)
	if e.nodeInfo != nil {
		name, tags = e.nodeInfo(id)
	}
	data, err := json.Marshal(silencesFrameData{Silences: e.silences.silencesForNode(now.Unix(), id, name, tags)})
	if err != nil {
		return
	}
	e.push(id, fleet.Frame{Type: "silences", Data: data})
}

// TickEscalations evaluates every currently firing (state=="firing" -- not
// acked, suppressed or resolved) incident's escalation schedule, called from
// masterLoop.tick alongside TickLeases/TickSilences. A no-op entirely when
// routing has never been wired (e.alerting == nil): escalation is a
// routing-config-driven feature with nothing to evaluate otherwise.
//
// The actual work runs inside a job enqueued on the incident's OWN keyed
// lane (fleet_dispatch.go), exactly like deliverUnsilenced/
// tryDeliverUnsilenced: by the time the job runs, the incident may have been
// acked or resolved by something else already queued ahead of it on that
// lane, so tryEscalate re-checks everything against the incident's CURRENT
// state, not this scan's snapshot.
func (e *fleetAlertEngine) TickEscalations(now time.Time) {
	if e.alerting == nil || e.incidents == nil {
		return
	}
	cfg := e.alerting.Get()
	for _, inc := range e.incidents.List(core.IncidentFilter{State: "firing"}, nil) {
		id, lane := inc.ID, inc.GroupKey
		e.dispatch.Enqueue(lane, func() { e.tryEscalate(id, cfg, now) })
	}
}

// escalatedTo reports whether inc's timeline already records step as
// escalated -- the escalation state is entirely durable, derived from the
// timeline (task-5 ruling), so a restarted master never re-sends a step it
// already reached.
func escalatedTo(inc core.Incident, step int) bool {
	prefix := fmt.Sprintf("step %d:", step)
	for _, ev := range inc.Timeline {
		if ev.Kind == "escalated" && strings.HasPrefix(ev.Detail, prefix) {
			return true
		}
	}
	return false
}

// firstFireDeliveryTS returns the timestamp of inc's fire leg's own
// "delivered" event (step 0, task-5 ruling: escalation timers are measured
// from the incident's first successful delivery), and whether one exists at
// all -- it may not yet, if the fire itself hasn't been delivered (e.g. still
// queued behind a slow channel, or every channel is currently failing).
func firstFireDeliveryTS(inc core.Incident) (int64, bool) {
	for _, ev := range inc.Timeline {
		if ev.Kind == "delivered" && strings.HasPrefix(ev.Detail, "fire: ") {
			return ev.TS, true
		}
	}
	return 0, false
}

// lastStepEventTS returns the most recent timestamp among: reaching step
// (its "delivered" event for step 0, or its "escalated" event otherwise) and
// any later "repeated" event recorded for that same step -- the reference
// point maybeRepeat measures RepeatEvery's cadence from, itself entirely
// derived from the timeline.
func lastStepEventTS(inc core.Incident, step int) int64 {
	var ts int64
	prefix := fmt.Sprintf("step %d:", step)
	bump := func(t int64) {
		if t > ts {
			ts = t
		}
	}
	for _, ev := range inc.Timeline {
		switch {
		case step == 0 && ev.Kind == "delivered" && strings.HasPrefix(ev.Detail, "fire: "):
			bump(ev.TS)
		case ev.Kind == "escalated" && strings.HasPrefix(ev.Detail, prefix):
			bump(ev.TS)
		case ev.Kind == "repeated" && strings.HasPrefix(ev.Detail, prefix):
			bump(ev.TS)
		}
	}
	return ts
}

// tryEscalate runs INSIDE id's keyed lane (enqueued by TickEscalations):
// re-reads the incident fresh (see TickEscalations's doc comment), and, if
// it is still firing with its fire leg delivered, delivers any step whose
// After has elapsed since that first delivery and that is not already
// recorded as escalated, then considers a RepeatEvery notification for
// whichever step was most recently reached.
func (e *fleetAlertEngine) tryEscalate(id string, cfg core.AlertingConfig, now time.Time) {
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State != "firing" || len(inc.Alerts) == 0 {
		return
	}
	al := inc.Alerts[len(inc.Alerts)-1]
	if al.ResolvedAt != 0 {
		return
	}
	firstTS, ok := firstFireDeliveryTS(inc)
	if !ok {
		return // the fire itself hasn't been delivered yet: nothing to escalate from.
	}
	name, tags := al.Node, []string(nil)
	if al.Node != "" && e.nodeInfo != nil {
		name, tags = e.nodeInfo(al.Node)
	}
	res := resolveRoute(cfg, al.Node, name, tags, al.Key, al.Severity)

	lastReached := 0
	for step := 1; step < len(res.Steps); step++ {
		if escalatedTo(inc, step) {
			lastReached = step
			continue
		}
		due, err := time.ParseDuration(res.Steps[step].After)
		if err != nil {
			continue
		}
		if now.Unix() < firstTS+int64(due/time.Second) {
			continue // not due yet -- steps need not be strictly ordered by After.
		}
		if e.escalateStep(id, step, res.Steps[step].Channels, now) {
			lastReached = step
			// Re-read: escalateStep just appended a timeline event.
			if updated, ok := e.incidents.Get(id); ok {
				inc = updated
			}
		}
	}
	e.maybeRepeat(id, inc, al, res, lastReached, now)
}

// escalateStep delivers channels for step through the same keyed dispatch
// (dispatchOnly: no alert-log/live-bus record -- an escalation is not a new
// alert), and, only on success, durably records
// {Kind:"escalated", Detail:"step N: chan1, chan2"} (task-5 ruling's literal
// format) so a restarted master never resends it (escalatedTo).
func (e *fleetAlertEngine) escalateStep(id string, step int, channels []string, now time.Time) bool {
	if e.dispatchOnly == nil {
		return false
	}
	a, ok := e.lastAlertOf(id)
	if !ok {
		return false
	}
	if !e.dispatchOnly(a, channels) {
		return false
	}
	_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
		TS: now.Unix(), Kind: "escalated", Detail: stepDetail(step, channels), Actor: "system",
	})
	return true
}

// maybeRepeat re-notifies lastReached's channels once RepeatEvery has
// elapsed since that step was last reached or last repeated (lastStepEventTS
// -- durable, timeline-derived), while inc is still firing and unacked
// (tryEscalate's caller already confirmed inc.State=="firing"). Recorded as
// {Kind:"repeated"} (task-5 ruling).
func (e *fleetAlertEngine) maybeRepeat(id string, inc core.Incident, al core.IncidentAlert, res routeResolution, lastReached int, now time.Time) {
	if res.RepeatEvery == "" || e.dispatchOnly == nil {
		return
	}
	every, err := time.ParseDuration(res.RepeatEvery)
	if err != nil || every <= 0 {
		return
	}
	ref := lastStepEventTS(inc, lastReached)
	if ref == 0 || now.Unix()-ref < int64(every/time.Second) {
		return
	}
	var channels []string
	if lastReached < len(res.Steps) {
		channels = res.Steps[lastReached].Channels
	}
	if len(channels) == 0 {
		return
	}
	sev, err := ParseSeverity(al.Severity)
	if err != nil {
		sev = SevWarning
	}
	a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}
	if !e.dispatchOnly(a, channels) {
		return
	}
	_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
		TS: now.Unix(), Kind: "repeated", Detail: stepDetail(lastReached, channels), Actor: "system",
	})
}

// lastAlertOf builds the Alert an escalation/repeat notification re-sends:
// id's most recently recorded alert instance, as an ordinary "fire" (an
// escalation only ever fires while the incident is still firing).
func (e *fleetAlertEngine) lastAlertOf(id string) (Alert, bool) {
	inc, ok := e.incidents.Get(id)
	if !ok || len(inc.Alerts) == 0 {
		return Alert{}, false
	}
	al := inc.Alerts[len(inc.Alerts)-1]
	sev, err := ParseSeverity(al.Severity)
	if err != nil {
		sev = SevWarning
	}
	return Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}, true
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
