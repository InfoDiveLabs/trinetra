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
	"regexp"
	"slices"
	"strconv"
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

// groupWait/groupInterval are task 6 part 2's incident-grouping timers
// (global-constraints: 30s/5m): the first delivery of a brand new incident
// waits groupWait since it opened, collecting members that fire in the
// meantime into ONE notification; a member that joins an already-delivered
// incident triggers at most one further "update" notification per
// groupInterval. Package VARS, not consts (task 6 brief: "so tests can
// shorten them") -- see tryDeliverGroup.
var (
	groupWait     = 30 * time.Second
	groupInterval = 5 * time.Minute
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

	// depsOf (task 6 part 3), wired once via SetDependencies, returns the
	// EXPANDED list of node ids a given node id depends on (any "tag:<t>"
	// entry already resolved against the current registry). A nil depsOf
	// (every existing call site/test that never calls SetDependencies) makes
	// dependencyFoldReason always report "no fold", exactly SetSilences's
	// nil-is-a-no-op pattern.
	depsOf func(nodeID string) []string
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

// ruleFromKey returns key's "rule" component for the default (rule,
// severity) group key (task 6 part 2's "the alert key with the node part
// stripped", defined precisely here): the part of the key that identifies
// WHICH NODE this specific alert instance is about, if any, stripped out, so
// the same underlying condition firing on different nodes shares one group.
//
// In this codebase, no alert key actually embeds the node it is ABOUT: a
// child-shipped alert's key (e.g. "cpu", "disk:/") names only the rule that
// fired, with its node carried separately as alertSource.NodeID, never as
// part of Key. The master's own per-target synthetic keys (e.g.
// "fleet:node:<id>:down") DO embed an id, but that id identifies which node
// the alert reports on, not "the node the alert came from" (a master-own
// alert has no source node at all: alertSource.NodeID is always "") -- so it
// is not "the node part" of an alert's own identity in the sense this
// grouping default cares about, and stripping it would silently default-
// group every currently-down node into one incident, which is not this
// codebase's behaviour (see the explicit, opt-in node-dependency fold in
// dependencyFoldReason for the one case where two different nodes' down
// alerts really do belong in the same incident). So ruleFromKey is the
// identity function today; it stays a named, precisely-defined seam so a
// future alert source whose key DOES embed its own node id has one obvious
// place to extend.
func ruleFromKey(key string) string { return key }

// groupKeyFor computes the (task 6 part 2) grouping bucket an alert with the
// given source/key/severity joins: an open incident whose OWN GroupKey
// equals this exact string is joined; otherwise a new incident opens with
// it. groupBy overrides the default (rule, severity) bucket with the
// caller-given field list (a matched route's GroupBy) -- each entry is one
// of "node", "rule", "severity", or "tag:<key>" (a boolean: does this
// alert's node carry that tag), rendered "field=value" and joined with "|"
// in the ORDER given, so the same fields always produce the same string
// regardless of any other alert's occurrence order.
func groupKeyFor(groupBy []string, src alertSource, a Alert) string {
	if len(groupBy) == 0 {
		return "rule=" + ruleFromKey(a.Key) + "|severity=" + a.Severity.String()
	}
	parts := make([]string, 0, len(groupBy))
	for _, g := range groupBy {
		switch {
		case g == "node":
			parts = append(parts, "node="+src.NodeID)
		case g == "rule":
			parts = append(parts, "rule="+ruleFromKey(a.Key))
		case g == "severity":
			parts = append(parts, "severity="+a.Severity.String())
		default:
			if tag, ok := strings.CutPrefix(g, "tag:"); ok {
				parts = append(parts, "tag:"+tag+"="+strconv.FormatBool(slices.Contains(src.Tags, tag)))
			}
		}
	}
	return strings.Join(parts, "|")
}

// groupKeyForRouted resolves src/a's matched route (exactly like real
// delivery -- resolveRoute) to find its GroupBy override, if any, then
// computes groupKeyFor. Used by Submit for every alert, so grouping and
// delivery routing can never disagree about which route matched.
func (e *fleetAlertEngine) groupKeyForRouted(src alertSource, a Alert) string {
	var cfg core.AlertingConfig
	if e.alerting != nil {
		cfg = e.alerting.Get()
	}
	res := resolveRoute(cfg, src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())
	return groupKeyFor(res.GroupBy, src, a)
}

// nodeDownKey returns the node-down alert key for nodeID -- the one place
// this exact format is built, matched by masterLoop.stillActive/
// checkOrphanedIncidents (fleet_daemon.go) and dependencyFoldReason below.
func nodeDownKey(nodeID string) string { return "fleet:node:" + nodeID + ":down" }

// dependencyFoldReason reports whether a node-down FIRE for nodeID (task 6
// part 3) should be folded into a dependency's own open node-down incident
// instead of delivered on its own: true when any of nodeID's (already
// tag-expanded) dependencies currently has an unresolved node-down alert.
// reason is the exact suppressed-member text ("suppressed: parent <name>
// down"); parentGroupKey is that dependency's own node-down alert's default
// group bucket (computed the SAME deterministic way its own fire opened/
// joined an incident with, since a node-down alert is never itself matched
// by a custom GroupBy route in any test/deployment this task covers), so the
// fold joins EXACTLY that incident. The first down dependency (in the
// node's own DependsOn order) wins if more than one currently qualifies.
func (e *fleetAlertEngine) dependencyFoldReason(nodeID string) (reason, parentGroupKey string, ok bool) {
	if e.depsOf == nil || e.incidents == nil {
		return "", "", false
	}
	for _, depID := range e.depsOf(nodeID) {
		if depID == "" || depID == nodeID {
			continue
		}
		key := nodeDownKey(depID)
		if !e.incidents.HasUnresolvedAlert("", key) {
			continue
		}
		name := depID
		if e.nodeInfo != nil {
			if n, _ := e.nodeInfo(depID); n != "" {
				name = n
			}
		}
		gk := groupKeyFor(nil, alertSource{}, Alert{Key: key, Severity: SevCritical})
		return "suppressed: parent " + name + " down", gk, true
	}
	return "", "", false
}

// releaseFoldedDependents runs after a node-down RECOVER is applied (task 6
// part 3): every currently folded (Suppressed != "", unresolved) member
// across every open incident is re-checked against dependencyFoldReason --
// if NONE of its dependencies are down any more, it is released ("mark the
// folded member as released") and delivered as its own, brand-new incident,
// exactly as if it had just fired on its own (task 6 part 3: "deliver the
// dependent's alert then, as its own incident"). recoveredNodeID is used
// only for the release reason text; the actual "still down?" check always
// re-derives from current incident state, so a member with MULTIPLE down
// dependencies is correctly left folded until every one of them clears.
func (e *fleetAlertEngine) releaseFoldedDependents(recoveredNodeID string, now int64) {
	if e.incidents == nil || e.depsOf == nil {
		return
	}
	recoveredName := recoveredNodeID
	if e.nodeInfo != nil {
		if n, _ := e.nodeInfo(recoveredNodeID); n != "" {
			recoveredName = n
		}
	}
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		for _, al := range inc.Alerts {
			if al.Suppressed == "" || al.ResolvedAt != 0 {
				continue
			}
			childID, isNodeDown := nodeDownTarget(al.Key)
			if !isNodeDown {
				continue
			}
			if _, _, stillFolded := e.dependencyFoldReason(childID); stillFolded {
				continue // some OTHER dependency is still down
			}
			reason := "released: parent " + recoveredName + " recovered"
			_, released, err := e.incidents.ReleaseFoldedMember(inc.ID, al.Node, al.Key, al.FiredAt, now, reason)
			if err != nil || !released {
				continue
			}
			e.deliverReleasedMember(al, now)
		}
	}
}

// deliverReleasedMember opens a BRAND NEW incident for a just-released
// dependency fold and enqueues its (grouped, but freshly opened so it never
// re-joins its former parent's bucket) delivery -- see
// releaseFoldedDependents's doc comment.
func (e *fleetAlertEngine) deliverReleasedMember(al core.IncidentAlert, now int64) {
	freshKey := fmt.Sprintf("released:%s:%s:%d", al.Node, al.Key, now)
	inc, err := e.incidents.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: al.Key, Title: al.Title, Severity: mustSeverity(al.Severity), Kind: "fire", Time: al.FiredAt},
		firedAt: al.FiredAt, now: now, groupKey: freshKey,
	})
	if err != nil {
		return
	}
	id := inc.ID
	e.dispatch.Enqueue(id, func() { e.tryDeliverGroup(id) })
}

// mustSeverity parses s, falling back to SevWarning for a bad/legacy value --
// this file's other IncidentAlert.Severity readers' exact rule.
func mustSeverity(s string) Severity {
	sev, err := ParseSeverity(s)
	if err != nil {
		return SevWarning
	}
	return sev
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

	// (task 6 part 3) A node-down FIRE is folded into a down dependency's own
	// incident, silently, instead of grouped/delivered on its own -- checked
	// only for an otherwise-deliverable fire (a locally-delivered or
	// silenced record has nothing left to fold).
	var depFold, depGroupKey string
	if a.Kind != "recover" && !deliveredLocally && supp == nil {
		if key, ok := nodeDownTarget(a.Key); ok {
			if reason, gk, folded := e.dependencyFoldReason(key); folded {
				depFold, depGroupKey = reason, gk
			}
		}
	}

	// (task 6 part 2) The grouping bucket this alert joins/opens: a
	// dependency fold always targets its parent's own incident's bucket
	// directly (see dependencyFoldReason), never the alert's OWN default/
	// routed bucket (which -- since ruleFromKey never merges different
	// node-down targets -- would otherwise always open a separate incident).
	groupKey := depGroupKey
	if groupKey == "" {
		groupKey = e.groupKeyForRouted(src, a)
	}

	// Step 1: durably record the decision before attempting delivery.
	var incidentID string
	if e.incidents != nil {
		inc, err := e.incidents.Apply(incidentApply{
			src: src, alert: a, firedAt: firedAt,
			deliveredLocally: deliveredLocally, suppressed: supp, now: e.now().Unix(),
			groupKey: groupKey, dependencyFold: depFold,
		})
		if err == nil {
			incidentID = inc.ID
		}
	}

	// (task 6 part 3) A node-down RECOVER may free up other nodes that were
	// folded into it: re-check every currently folded member regardless of
	// whether THIS recover itself ends up delivered/suppressed (the
	// underlying "is it still down" fact just changed either way).
	if a.Kind == "recover" {
		if recoveredID, ok := nodeDownTarget(a.Key); ok {
			e.releaseFoldedDependents(recoveredID, e.now().Unix())
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

	// The dispatcher lane key is the incident id (task 6 part 2: "the keyed
	// lane key becomes the incident ID"), so every job touching this
	// incident -- this fire/recover, another member's fire/recover, a group
	// notification, an escalation -- runs strictly in the order it was
	// enqueued, never just this one (node, key)'s own legs. incidentID can
	// only be "" when e.incidents itself is nil (no store at all, e.g. a
	// bare-bones test): incidentGroupKey is then just a stable, arbitrary
	// per-(node,key) lane so ordering still holds for that one pair.
	lane := incidentID
	if lane == "" {
		lane = incidentGroupKey(src.NodeID, a.Key)
	}

	if a.Kind == "recover" {
		e.dispatch.Enqueue(lane, func() {
			e.handleRecover(src, a, firedAt, incidentID)
		})
		return
	}

	if depFold != "" {
		// Folded: recorded above as a suppressed member, never delivered or
		// counted toward the incident's own group notification.
		return
	}

	e.dispatch.Enqueue(lane, func() {
		e.tryDeliverGroup(incidentID)
	})
}

// nodeDownTarget reports the target node id embedded in a master-own
// node-down alert key, per nodeDownKey's exact format.
func nodeDownTarget(key string) (string, bool) {
	if strings.HasPrefix(key, "fleet:node:") && strings.HasSuffix(key, ":down") {
		return strings.TrimSuffix(strings.TrimPrefix(key, "fleet:node:"), ":down"), true
	}
	return "", false
}

// deliverAndReceipt is Submit's steps 2-4, run off Submit's own goroutine
// (via the keyed dispatcher): attempt delivery, and ONLY on success record
// the "delivered" timeline event and push the receipt.
func (e *fleetAlertEngine) deliverAndReceipt(src alertSource, a Alert, firedAt int64, incidentID string) {
	e.deliverAndReceiptDetail(src, a, firedAt, incidentID, "sent via the master's dispatcher")
}

// handleRecover is a recover leg's own dispatch job (task 6 part 2): an
// incident with a still-open OTHER member is not over yet, so this member's
// own recover gets no human-facing notification -- only its receipt, so its
// own child does not fall back and re-deliver a duplicate local recover
// (the at-least-once/dedup contract is per (node, key, fired_at), not per
// incident). Only once EVERY member has recovered (incidentStore.Apply
// already decided this: inc.State == "resolved") does the ordinary,
// immediate ("as today", no group_wait/interval) resolved delivery happen.
func (e *fleetAlertEngine) handleRecover(src alertSource, a Alert, firedAt int64, incidentID string) {
	resolved := incidentID == "" // no incident store at all: behave as before grouping existed.
	if incidentID != "" && e.incidents != nil {
		if inc, ok := e.incidents.Get(incidentID); ok {
			resolved = inc.State == "resolved"
		} else {
			resolved = true // shouldn't happen; don't strand the receipt.
		}
	}
	if resolved {
		e.deliverAndReceipt(src, a, firedAt, incidentID)
		return
	}
	if src.NodeID != "" && e.push != nil {
		e.pushReceipt(src.NodeID, a.Key, firedAt)
	}
}

// tryDeliverGroup runs INSIDE id's own keyed lane (enqueued by Submit for
// every fire, and by TickGrouping's periodic re-check): the master's
// incident-grouping delivery (task 6 part 2). It re-reads the incident
// fresh -- by the time this job runs, an arbitrary amount of time may have
// passed and another job for the same incident (another member's fire, this
// one's own eventual recover) may already have run ahead of it, exactly like
// tryDeliverUnsilenced/tryEscalate.
//
// pending is every member that has never had a fire delivered for it yet
// (legDeliveredStatusFor), excluding a dependency-suppressed one (never
// delivered via grouping at all -- see dependencyFoldReason). If there is
// nothing pending, there is nothing to do (a plain re-fire of an
// already-delivered member, or every member already covered). Otherwise:
// with NO delivery yet at all, this is the incident's very first
// notification, gated by groupWait since it opened; with at least one
// already delivered, this is an "update" adding pending's members, gated by
// groupInterval since the LAST group delivery -- both durably derived from
// the timeline (lastGroupDeliveryTS), never from in-memory state, so a
// restarted master picks up exactly where it left off.
func (e *fleetAlertEngine) tryDeliverGroup(id string) {
	if id == "" || e.incidents == nil {
		return
	}
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State == "suppressed" {
		return // silenced: never group-delivered.
	}
	// pending is deliberately NOT filtered by ResolvedAt (B3-style race, see
	// TestEngineFireThenRecoverDeliveredInOrderEvenOnSlowChannel): Apply
	// records a recover SYNCHRONOUSLY, in the caller's own goroutine, the
	// instant Submit is called for it -- entirely independent of when this
	// deferred, lane-queued job happens to run. A member that has ALREADY
	// recovered by the time its OWN fire notification finally goes out must
	// still get that fire notification (its keyed lane guarantees the
	// recover's own dispatch job runs strictly after this one, so ordering
	// is preserved regardless) -- only a member whose fire was already
	// delivered is excluded.
	var pending []core.IncidentAlert
	anyDelivered := false
	for _, al := range inc.Alerts {
		if al.Suppressed != "" {
			continue
		}
		if fireDelivered, _ := legDeliveredStatusFor(inc, al.Node, al.Key, al.FiredAt); fireDelivered {
			anyDelivered = true
			continue
		}
		pending = append(pending, al)
	}
	if len(pending) == 0 {
		return
	}
	now := e.now()
	if !anyDelivered {
		if now.Before(time.Unix(inc.Opened, 0).Add(groupWait)) {
			return // still collecting members for the first notification.
		}
	} else if last := lastGroupDeliveryTS(inc); now.Before(time.Unix(last, 0).Add(groupInterval)) {
		return // an update was sent too recently; the next tick will catch this.
	}
	e.deliverGroup(inc, pending, now.Unix())
}

// lastGroupDeliveryTS returns the most recent timestamp among every member's
// fire-leg "delivered" event -- the reference point tryDeliverGroup measures
// groupInterval's cadence from, entirely derived from the timeline.
func lastGroupDeliveryTS(inc core.Incident) int64 {
	var ts int64
	for _, ev := range inc.Timeline {
		if ev.Kind == "delivered" && ev.Leg == "fire" && ev.TS > ts {
			ts = ev.TS
		}
	}
	return ts
}

// memberNodeName returns al's node's display name: al.NodeName as recorded
// AT FIRE TIME (task 6 part 2 -- the exact name Submit's caller passed in,
// matching how a single, non-grouped alert's title was always built from
// src.NodeName directly, never a registry lookup), falling back to
// e.nodeInfo (a CURRENT registry lookup, for a legacy IncidentAlert with no
// NodeName recorded) and finally the bare node id.
func (e *fleetAlertEngine) memberNodeName(al core.IncidentAlert) string {
	if al.NodeName != "" {
		return al.NodeName
	}
	if e.nodeInfo != nil {
		if n, _ := e.nodeInfo(al.Node); n != "" {
			return n
		}
	}
	return al.Node
}

// groupAlertTitle builds the single notification's title for pending's
// members: a lone member keeps today's exact "<node>: <title>" shape (or the
// bare title for a master-own alert); 2+ members are listed by name, so one
// notification covers all of them (task 6 part 2: "sends ONE message listing
// all member nodes" / "...listing the new members").
func (e *fleetAlertEngine) groupAlertTitle(inc core.Incident, pending []core.IncidentAlert) string {
	if len(pending) == 1 {
		al := pending[0]
		if al.Node == "" {
			return al.Title
		}
		return e.memberNodeName(al) + ": " + al.Title
	}
	names := make([]string, 0, len(pending))
	for _, al := range pending {
		name := "self"
		if al.Node != "" {
			name = e.memberNodeName(al)
		}
		names = append(names, name)
	}
	return fmt.Sprintf("%s (%d nodes): %s", inc.Title, len(pending), strings.Join(names, ", "))
}

// deliverGroup is tryDeliverGroup's actual send: ONE physical delivery
// covering every member in pending, and, only on success, one structured
// "delivered" timeline event PER member (so legDeliveredStatusFor/
// resurrection/further grouping checks see each of them as covered) and one
// receipt pushed to each member's own node (task 6 part 2: "receipts are
// still per child alert... after the delivery that included it").
func (e *fleetAlertEngine) deliverGroup(inc core.Incident, pending []core.IncidentAlert, now int64) {
	sev, err := ParseSeverity(inc.Severity)
	if err != nil {
		sev = SevWarning
	}
	title := e.groupAlertTitle(inc, pending)
	lead := pending[0]
	da := Alert{Key: lead.Key, Title: title, Severity: sev, Kind: "fire", Time: now}

	var ok bool
	var record func()
	switch {
	case e.alerting != nil && e.deliverNamed != nil:
		cfg := e.alerting.Get()
		name, tags := lead.Node, []string(nil)
		if lead.Node != "" && e.nodeInfo != nil {
			name, tags = e.nodeInfo(lead.Node)
		}
		res := resolveRoute(cfg, lead.Node, name, tags, lead.Key, lead.Severity)
		channels := unionStepChannels(res.Policies, 0)
		ok = e.deliverNamed(da, channels)
		record = func() {
			for _, al := range pending {
				for _, p := range res.Policies {
					if len(p.Steps) == 0 {
						continue
					}
					_, _ = e.incidents.AppendEvent(inc.ID, core.IncidentEvent{
						TS: now, Kind: "delivered", Detail: "fire: " + policyStepDetail(p.Name, 0, p.Steps[0].Channels), Actor: "system",
						Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
						Policy: p.Name, Step: 0, Channels: append([]string(nil), p.Steps[0].Channels...),
					})
				}
			}
		}
	case e.deliver != nil:
		ok = e.deliver(da)
		record = func() {
			for _, al := range pending {
				_, _ = e.incidents.AppendEvent(inc.ID, core.IncidentEvent{
					TS: now, Kind: "delivered", Detail: "fire: sent via the master's dispatcher (grouped)", Actor: "system",
					Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
				})
			}
		}
	default:
		return
	}
	if !ok {
		return // no channel accepted it: no receipts, no "delivered" events.
	}
	record()
	for _, al := range pending {
		if al.Node != "" && e.push != nil {
			e.pushReceipt(al.Node, al.Key, al.FiredAt)
		}
	}
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

	if e.alerting == nil {
		// Pre-routing behaviour, byte for byte: every existing call site/test
		// that never wires SetRouting never sees anything below this branch.
		if a.Kind == "recover" && !e.sendResolved {
			return
		}
		if e.deliver == nil {
			return
		}
		if !e.deliver(da) {
			return // no channel accepted it: no receipt, no "delivered" event.
		}
		if e.incidents != nil && incidentID != "" {
			_, _ = e.incidents.AppendEvent(incidentID, core.IncidentEvent{
				TS: e.now().Unix(), Kind: "delivered", Detail: legLabel(a) + ": " + note, Actor: "system",
				Leg: legLabel(a), AlertKey: a.Key, Node: src.NodeID, FiredAt: firedAt,
			})
		}
		if src.NodeID != "" && e.push != nil {
			e.pushReceipt(src.NodeID, a.Key, firedAt)
		}
		return
	}

	// Routing/escalation (B5 fix round 1): resolve the SAME way RouteTest
	// does (resolveRoute) -- EVERY matched policy applies independently (see
	// routeResolution's doc comment), so from here on there is no single
	// "the" policy/step list any more.
	if e.deliverNamed == nil {
		return
	}
	cfg := e.alerting.Get()
	res := resolveRoute(cfg, src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())

	if a.Kind == "recover" {
		e.deliverResolved(src, da, firedAt, incidentID, res.Policies)
		return
	}

	// fire: one physical dispatch to the union of every matched policy's
	// step 0 (ruling: "in one dispatch, as now"), but one timeline event PER
	// policy, so later escalation/repeat/resolved-union bookkeeping can
	// attribute each channel to the policy that actually asked for it.
	channels := unionStepChannels(res.Policies, 0)
	if !e.deliverNamed(da, channels) {
		return // no channel accepted it: no receipt, no "delivered" event.
	}
	if e.incidents != nil && incidentID != "" {
		ts := e.now().Unix()
		for _, p := range res.Policies {
			if len(p.Steps) == 0 {
				continue
			}
			_, _ = e.incidents.AppendEvent(incidentID, core.IncidentEvent{
				TS: ts, Kind: "delivered", Detail: legLabel(a) + ": " + policyStepDetail(p.Name, 0, p.Steps[0].Channels), Actor: "system",
				Leg: legLabel(a), AlertKey: a.Key, Node: src.NodeID, FiredAt: firedAt,
				Policy: p.Name, Step: 0, Channels: append([]string(nil), p.Steps[0].Channels...),
			})
		}
	}
	if src.NodeID != "" && e.push != nil {
		e.pushReceipt(src.NodeID, a.Key, firedAt)
	}
}

// deliverResolved delivers the recover leg once routing is wired: the
// resolved message goes to the union of every channel that received a step
// (fire, escalated or repeated) from a policy whose SendResolved is true
// (default true) -- task-5/B5 ruling. If history has nothing to derive that
// from (e.g. a resurrection edge case), it falls back to the union of step
// 0 across every SendResolved-true matched policy, so a legitimate resolved
// message is never silently dropped just because history was incomplete. If
// NO matched policy wants it sent at all, nothing is delivered (matching the
// old single-policy !SendResolved early return).
func (e *fleetAlertEngine) deliverResolved(src alertSource, da Alert, firedAt int64, incidentID string, policies []core.Policy) {
	channels := e.unionResolvedChannels(incidentID, policies)
	if len(channels) == 0 {
		for _, p := range policies {
			if len(p.Steps) == 0 || !sendResolvedOf(p) {
				continue
			}
			channels = append(channels, p.Steps[0].Channels...)
		}
		channels = dedupStrings(channels)
	}
	if len(channels) == 0 {
		return
	}
	if !e.deliverNamed(da, channels) {
		return
	}
	if e.incidents != nil && incidentID != "" {
		_, _ = e.incidents.AppendEvent(incidentID, core.IncidentEvent{
			TS: e.now().Unix(), Kind: "delivered", Detail: legLabel(da) + ": " + strings.Join(channels, ", "), Actor: "system",
			Leg: legLabel(da), AlertKey: da.Key, Node: src.NodeID, FiredAt: firedAt, Channels: append([]string(nil), channels...),
		})
	}
	if src.NodeID != "" && e.push != nil {
		e.pushReceipt(src.NodeID, da.Key, firedAt)
	}
}

// policyStepDetail formats a step-N delivery/escalation/repeat timeline
// event's Detail: "policy P step N: chan1, chan2" (B5 fix round 1 ruling) --
// used for every routed per-policy delivery (fire's own step 0, escalated,
// repeated) so stepEventInfo can parse them all the same way and attribute
// each to the policy that produced it.
func policyStepDetail(policy string, step int, channels []string) string {
	return fmt.Sprintf("policy %s step %d: %s", policy, step, strings.Join(channels, ", "))
}

// stepEventDetailRe/legacyStepDetailRe parse policyStepDetail's format, and
// its PRE-fix-round-1 predecessor ("step N: chan1, chan2", no policy name --
// B5 ruling: "legacy events with no policy prefix: treat them as belonging
// to the first matched policy"). A policy name is taken greedily up to the
// LAST " step N: " it could possibly precede, so a policy name that
// contained the literal substring " step " (unlikely -- policy names are
// short identifiers) would still parse correctly.
var (
	stepEventDetailRe  = regexp.MustCompile(`^policy (.+) step (\d+): (.*)$`)
	legacyStepDetailRe = regexp.MustCompile(`^step (\d+): (.*)$`)
)

// stepEventInfo extracts (policy, step, channels) from a "delivered" (fire
// leg only), "escalated" or "repeated" timeline event. Every event this
// engine writes now carries this structurally (Policy/Step/Channels, task 6
// part 1): when ev.Policy is set, it is used directly, with no text parsing
// at all -- this is what fixes the review finding that a policy or channel
// name containing the literal substring " step N: " could confuse the old
// text-only parser (see policyStepDetail's format). Detail is still written
// for display, but never consulted here for a structured event.
//
// ev.Policy == "" falls back to parsing Detail exactly as this engine did
// before structured fields existed, for a LEGACY event recorded by an older
// build: policy is "" for one recorded before per-policy labelling existed
// at all (caller attributes it to the first matched policy -- see
// escalatedTo/lastStepEventTS). ok is false for anything else (a recover's
// own "delivered" event, an unrelated event kind, or a malformed detail).
func stepEventInfo(ev core.IncidentEvent) (policy string, step int, channels []string, ok bool) {
	if ev.Policy != "" {
		switch ev.Kind {
		case "delivered":
			if ev.Leg != "fire" {
				return "", 0, nil, false
			}
		case "escalated", "repeated":
		default:
			return "", 0, nil, false
		}
		return ev.Policy, ev.Step, append([]string(nil), ev.Channels...), true
	}

	detail := ev.Detail
	switch ev.Kind {
	case "delivered":
		var hasFire bool
		detail, hasFire = strings.CutPrefix(ev.Detail, "fire: ")
		if !hasFire {
			return "", 0, nil, false
		}
	case "escalated", "repeated":
		// detail already set above
	default:
		return "", 0, nil, false
	}
	if m := stepEventDetailRe.FindStringSubmatch(detail); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return "", 0, nil, false
		}
		return m[1], n, splitChannelList(m[3]), true
	}
	if m := legacyStepDetailRe.FindStringSubmatch(detail); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return "", 0, nil, false
		}
		return "", n, splitChannelList(m[2]), true
	}
	return "", 0, nil, false
}

// splitChannelList splits a comma-and-space-joined channel list back into
// its entries (the inverse of strings.Join(channels, ", ")).
func splitChannelList(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ", ") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// unionResolvedChannels returns the deduplicated, order-preserving union of
// every channel from a step-event (the fire leg's own step 0, any
// escalation, any repeat) whose OWNING POLICY has SendResolved true --
// default true for a policy no longer present in the current matched set
// (e.g. the config changed mid-incident): B5 ruling, "the union of channels
// that received steps from policies with SendResolved=true".
func (e *fleetAlertEngine) unionResolvedChannels(incidentID string, policies []core.Policy) []string {
	if incidentID == "" || e.incidents == nil {
		return nil
	}
	inc, ok := e.incidents.Get(incidentID)
	if !ok {
		return nil
	}
	sendResolved := map[string]bool{}
	for _, p := range policies {
		sendResolved[p.Name] = sendResolvedOf(p)
	}
	firstPolicy := ""
	if len(policies) > 0 {
		firstPolicy = policies[0].Name
	}
	seen := map[string]bool{}
	var out []string
	for _, ev := range inc.Timeline {
		policy, _, channels, ok := stepEventInfo(ev)
		if !ok {
			continue
		}
		if policy == "" {
			policy = firstPolicy
		}
		if sr, known := sendResolved[policy]; known && !sr {
			continue
		}
		for _, c := range channels {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
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

// legDeliveredStatusFor is legDeliveredStatus scoped to ONE member alert
// (task 6 part 2: "delivery status... must work PER IncidentAlert, matched
// by (Node, AlertKey, FiredAt) through the structured fields"), for a
// grouped incident whose members may have independent delivery histories
// (see TestEngineResurrectionOnlyResendsUndeliveredMember). Every event this
// engine writes now carries Leg/AlertKey/Node/FiredAt (task 6 part 1); an
// event with Leg set is matched against exactly this member and ignored
// otherwise. An event with Leg == "" is a LEGACY event (recorded before
// these fields existed) and is matched the OLD, incident-wide way
// (legDeliveredStatus's exact rule) with no node/key/firedAt filtering at
// all -- correct because a legacy incident, recorded before grouping
// existed, always has exactly one member, so "the incident's timeline" and
// "this member's timeline" are the same thing.
func legDeliveredStatusFor(inc core.Incident, node, key string, firedAt int64) (fireDelivered, recoverDelivered bool) {
	for _, ev := range inc.Timeline {
		if ev.Kind != "delivered" && ev.Kind != "suppressed" {
			continue
		}
		if ev.Leg != "" {
			if ev.Node != node || ev.AlertKey != key || ev.FiredAt != firedAt {
				continue
			}
			switch ev.Leg {
			case "fire":
				fireDelivered = true
			case "recover":
				recoverDelivered = true
			}
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
// start"): master-own alerts (Node == "" on the IncidentAlert itself, e.g.
// node-down/connectivity) have no child-side fallback if the master crashes
// between recording a fire/recover (Submit's step 1) and actually delivering
// it -- unlike a child's own alert, nothing else will ever retry it.
//
// It checks EVERY master-own member of EVERY incident independently (task 6
// part 2: a grouped or dependency-folded incident can hold several master-own
// members with entirely independent delivery histories -- see
// TestEngineResurrectionOnlyResendsUndeliveredMember), skipping a
// dependency-suppressed one entirely (Suppressed != "": deliberately never
// delivered on its own -- see dependencyFoldReason/releaseFoldedDependents).
// For each remaining member, the FIRE and RECOVER legs are checked
// independently (B3 review round 3: a single check based only on the
// incident's current state/last timeline entry could miss an undelivered
// fire when the recover, recorded later, is the one that happens to look
// "undelivered"): for a member younger than 24h, any leg with no matching
// "delivered" event (legDeliveredStatusFor -- structured-first, task 6 part
// 1) is re-enqueued on the incident's own keyed lane (its id), fire before
// recover, so ordering is preserved exactly as an ordinary Submit call would
// produce it. 24h or older, it gives up on whichever leg(s) are still
// undelivered and records why, once, without delivering anything -- this
// give-up event deliberately carries no Leg (see legDeliveredStatusFor's
// legacy-fallback branch: an unlabelled event is never mistaken for
// "delivered"), exactly matching its pre-structured-fields shape.
func (e *fleetAlertEngine) resurrectMasterAlerts() {
	if e.incidents == nil {
		return
	}
	now := e.now()
	cutoff := now.Add(-24 * time.Hour).Unix()
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		id := inc.ID
		for _, al := range inc.Alerts {
			if al.Node != "" || al.Suppressed != "" {
				continue // a child-sourced or dependency-folded member: not this function's job
			}
			hasRecover := al.ResolvedAt != 0
			fireDelivered, recoverDelivered := legDeliveredStatusFor(inc, al.Node, al.Key, al.FiredAt)
			if fireDelivered && (!hasRecover || recoverDelivered) {
				continue // both recorded legs already confirmed delivered
			}

			if inc.Updated < cutoff {
				_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
					TS: now.Unix(), Kind: "suppressed", Detail: "not delivered: master restarted", Actor: "system",
					AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
				})
				continue
			}

			sev, err := ParseSeverity(al.Severity)
			if err != nil {
				sev = SevWarning
			}
			al := al
			if !fireDelivered {
				a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}
				firedAt := al.FiredAt
				e.dispatch.Enqueue(id, func() {
					e.deliverAndReceiptDetail(alertSource{}, a, firedAt, id, "redelivered after restart")
				})
			}
			if hasRecover && !recoverDelivered {
				a := Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "recover", Time: al.ResolvedAt}
				firedAt := al.ResolvedAt
				// Enqueued on the SAME lane as the fire above (the incident's
				// own id), so the keyed dispatcher's per-lane FIFO guarantees
				// the recover never runs first even though both were just
				// enqueued back to back here.
				e.dispatch.Enqueue(id, func() {
					e.deliverAndReceiptDetail(alertSource{}, a, firedAt, id, "redelivered after restart")
				})
			}
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
		// (task 6 part 2) Looked up by the specific (node, key) MEMBER, not
		// the incident's own grouping bucket: a grouped incident's bucket key
		// no longer has anything to do with (node, key) by default.
		inc, ok := e.incidents.OpenAlertIncident(nodeID, key)
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

// SetDependencies wires the engine to a node-dependency resolver (task 6
// part 3), mirroring SetSilences/SetRouting's pattern: called once from
// startMaster, after the engine exists. depsOf(id) must return id's
// DependsOn list already expanded (every "tag:<t>" entry resolved to the
// concrete node ids currently carrying that tag). A nil depsOf (never
// called, e.g. every pre-existing engine test) keeps Submit's node-down
// handling exactly as it behaved before dependencies existed.
func (e *fleetAlertEngine) SetDependencies(depsOf func(nodeID string) []string) {
	e.depsOf = depsOf
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
		id := inc.ID
		e.dispatch.Enqueue(id, func() { e.tryDeliverUnsilenced(id) })
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
		id := inc.ID
		e.dispatch.Enqueue(id, func() { e.tryEscalate(id, cfg, now) })
	}
}

// TickGrouping re-checks every open (firing or acked) incident's pending
// group notification (task 6 part 2): a fire always tries tryDeliverGroup
// itself immediately (Submit), but that first attempt can find group_wait
// (or group_interval, for an update) not yet elapsed and give up -- nothing
// else would ever retry it without this periodic sweep, called from
// masterLoop.tick alongside TickLeases/TickSilences/TickEscalations.
// tryDeliverGroup itself is cheap to call speculatively (it is a fast no-op
// whenever there is nothing pending or nothing due yet), so this scans every
// open incident rather than maintaining a separate "has a pending
// notification" index.
func (e *fleetAlertEngine) TickGrouping(now time.Time) {
	if e.incidents == nil {
		return
	}
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		if inc.State != "firing" && inc.State != "acked" {
			continue
		}
		id := inc.ID
		e.dispatch.Enqueue(id, func() { e.tryDeliverGroup(id) })
	}
}

// escalatedTo reports whether inc's timeline already records (policy, step)
// as escalated -- the escalation state is entirely durable, derived from the
// timeline (task-5 ruling), so a restarted master never re-sends a step it
// already reached. isFirst attributes a legacy, pre-policy-labelling event
// (B5 fix round 1: recorded by an engine build before per-policy escalation
// existed) to policy when policy is the first matched policy -- see
// stepEventInfo.
func escalatedTo(inc core.Incident, policy string, step int, isFirst bool) bool {
	for _, ev := range inc.Timeline {
		if ev.Kind != "escalated" {
			continue
		}
		p, n, _, ok := stepEventInfo(ev)
		if !ok || n != step {
			continue
		}
		if p == policy || (p == "" && isFirst) {
			return true
		}
	}
	return false
}

// firstFireDeliveryTS returns the timestamp of inc's fire leg's own step-0
// "delivered" event (task-5 ruling: escalation timers are measured from the
// incident's first successful delivery -- ONE shared reference time across
// every matched policy, per B5's ruling), and whether one exists at all --
// it may not yet, if the fire itself hasn't been delivered (e.g. still
// queued behind a slow channel, or every channel is currently failing).
// Every matched policy's own step-0 "delivered" event is appended with the
// SAME timestamp (deliverAndReceiptDetail's fire branch), so it does not
// matter which one this happens to find first.
func firstFireDeliveryTS(inc core.Incident) (int64, bool) {
	for _, ev := range inc.Timeline {
		if ev.Kind != "delivered" {
			continue
		}
		if ev.Leg != "" {
			if ev.Leg == "fire" {
				return ev.TS, true
			}
			continue
		}
		if strings.HasPrefix(ev.Detail, "fire: ") {
			return ev.TS, true
		}
	}
	return 0, false
}

// lastStepEventTS returns the most recent timestamp among: policy reaching
// step (its step-0 "delivered" event, or its "escalated" event otherwise)
// and any later "repeated" event recorded for that same (policy, step) --
// the reference point maybeRepeat measures RepeatEvery's cadence from,
// itself entirely derived from the timeline. isFirst is escalatedTo's
// legacy-event attribution rule.
func lastStepEventTS(inc core.Incident, policy string, step int, isFirst bool) int64 {
	var ts int64
	for _, ev := range inc.Timeline {
		if ev.Kind != "delivered" && ev.Kind != "escalated" && ev.Kind != "repeated" {
			continue
		}
		p, n, _, ok := stepEventInfo(ev)
		if !ok || n != step {
			continue
		}
		if (p == policy || (p == "" && isFirst)) && ev.TS > ts {
			ts = ev.TS
		}
	}
	return ts
}

// tryEscalate runs INSIDE id's keyed lane (enqueued by TickEscalations):
// re-reads the incident fresh (see TickEscalations's doc comment), and, if
// it is still firing with its fire leg delivered, walks EVERY matched
// policy independently (B5 fix round 1 ruling: each policy has its own
// steps, After durations, RepeatEvery and SendResolved -- there is no
// merging), delivering any step whose After has elapsed since the shared
// first-delivery time and that is not already recorded as escalated for
// THAT policy, then considers a RepeatEvery notification for whichever step
// that policy most recently reached.
//
// PARKED (B5 review, same precedent as unsilence's tryDeliverUnsilenced): an
// ack or resolve can land on this incident, from a different code path,
// between the state check above and an escalateStep/dispatchOnly call
// below -- a step already in flight can still be delivered a moment after
// the operator acked it. Accepted, not fixed here.
func (e *fleetAlertEngine) tryEscalate(id string, cfg core.AlertingConfig, now time.Time) {
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State != "firing" || len(inc.Alerts) == 0 {
		return
	}
	// (task 6 part 2) A grouped incident's escalation is driven by its most
	// recently fired member that is still active (unresolved and not
	// dependency-suppressed) -- NOT necessarily the last-appended Alerts
	// entry, which for a grouped incident may already have recovered while
	// an earlier member is still open.
	al, ok := latestActiveAlert(inc)
	if !ok {
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

	for pi, p := range res.Policies {
		isFirst := pi == 0
		lastReached := 0
		for step := 1; step < len(p.Steps); step++ {
			if escalatedTo(inc, p.Name, step, isFirst) {
				lastReached = step
				continue
			}
			due, err := time.ParseDuration(p.Steps[step].After)
			if err != nil {
				continue
			}
			if now.Unix() < firstTS+int64(due/time.Second) {
				continue // not due yet -- steps need not be strictly ordered by After.
			}
			if e.escalateStep(id, al, p.Name, step, p.Steps[step].Channels, now) {
				lastReached = step
				// Re-read: escalateStep just appended a timeline event.
				if updated, ok := e.incidents.Get(id); ok {
					inc = updated
				}
			}
		}
		e.maybeRepeat(id, inc, al, p, isFirst, lastReached, now)
	}
}

// escalateStep delivers channels for (policy, step) through the same keyed
// dispatch (dispatchOnly: no alert-log/live-bus record -- an escalation is
// not a new alert), and, only on success, durably records
// {Kind:"escalated", Detail:"policy P step N: chan1, chan2"} (B5 fix round 1
// ruling) so a restarted master never resends it (escalatedTo).
func (e *fleetAlertEngine) escalateStep(id string, al core.IncidentAlert, policy string, step int, channels []string, now time.Time) bool {
	if e.dispatchOnly == nil {
		return false
	}
	a, ok := alertOf(al)
	if !ok {
		return false
	}
	if !e.dispatchOnly(a, channels) {
		return false
	}
	_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
		TS: now.Unix(), Kind: "escalated", Detail: policyStepDetail(policy, step, channels), Actor: "system",
		Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
		Policy: policy, Step: step, Channels: append([]string(nil), channels...),
	})
	return true
}

// maybeRepeat re-notifies p's last-reached step's channels once p's OWN
// RepeatEvery has elapsed since that step was last reached or last repeated
// for p (lastStepEventTS -- durable, timeline-derived), while inc is still
// firing and unacked (tryEscalate's caller already confirmed
// inc.State=="firing"). Recorded as {Kind:"repeated"} (task-5 ruling); each
// matched policy is considered independently (B5 fix round 1), with its own
// cadence and its own last-reached step.
func (e *fleetAlertEngine) maybeRepeat(id string, inc core.Incident, al core.IncidentAlert, p core.Policy, isFirst bool, lastReached int, now time.Time) {
	if p.RepeatEvery == "" || e.dispatchOnly == nil {
		return
	}
	every, err := time.ParseDuration(p.RepeatEvery)
	if err != nil || every <= 0 {
		return
	}
	ref := lastStepEventTS(inc, p.Name, lastReached, isFirst)
	if ref == 0 || now.Unix()-ref < int64(every/time.Second) {
		return
	}
	var channels []string
	if lastReached < len(p.Steps) {
		channels = p.Steps[lastReached].Channels
	}
	if len(channels) == 0 {
		return
	}
	a, ok := alertOf(al)
	if !ok {
		return
	}
	if !e.dispatchOnly(a, channels) {
		return
	}
	_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
		TS: now.Unix(), Kind: "repeated", Detail: policyStepDetail(p.Name, lastReached, channels), Actor: "system",
		Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
		Policy: p.Name, Step: lastReached, Channels: append([]string(nil), channels...),
	})
}

// alertOf rebuilds the ordinary "fire" Alert an escalation/repeat
// notification re-sends from a recorded IncidentAlert (an escalation only
// ever fires while the incident is still firing). ok is false only for a
// malformed severity that ParseSeverity itself rejects... which cannot
// actually happen (a bad severity falls back to SevWarning, same as every
// other reader of IncidentAlert.Severity elsewhere in this file) -- kept as
// a (T, bool) return to match this file's other alert-rebuilding helpers.
func alertOf(al core.IncidentAlert) (Alert, bool) {
	sev, err := ParseSeverity(al.Severity)
	if err != nil {
		sev = SevWarning
	}
	return Alert{Key: al.Key, Title: al.Title, Severity: sev, Kind: "fire", Time: al.FiredAt}, true
}

// latestActiveAlert returns inc's most recently fired member alert that is
// still active -- unresolved and not dependency-suppressed (task 6 parts 2
// and 3): what escalation/repeat notifications and tryEscalate's route
// resolution key off, since a grouped incident's LAST-APPENDED Alerts entry
// is not necessarily the one still open (an earlier member can still be
// firing after a later one already recovered).
func latestActiveAlert(inc core.Incident) (core.IncidentAlert, bool) {
	var best core.IncidentAlert
	found := false
	for _, a := range inc.Alerts {
		if a.ResolvedAt != 0 || a.Suppressed != "" {
			continue
		}
		if !found || a.FiredAt > best.FiredAt {
			best, found = a, true
		}
	}
	return best, found
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
