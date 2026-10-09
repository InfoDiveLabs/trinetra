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

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/telegram"
)

// incidentButtons builds the inline keyboard attached to an incident's fire notification:
// Ack and a 1-hour silence, both carrying the incident id verbatim in their callback data.
func incidentButtons(incidentID string) [][]telegram.Button {
	if incidentID == "" {
		return nil
	}
	return [][]telegram.Button{{
		{Text: "Ack", Data: "ack:" + incidentID},
		{Text: "Silence 1h", Data: "sil1h:" + incidentID},
	}}
}

// leaseInterval/leaseValidFor are the lease cadence (global-constraints:
// sent every 30s, valid 90s).
const (
	leaseInterval = 30 * time.Second
	leaseValidFor = 90 * time.Second
)

// groupWait/groupInterval are the incident-grouping timers: the first delivery of a new
// incident waits groupWait since it opened.
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

// alertDedupKey is the master's dedup key for one alert record: (node_id, alert_key,
// fired_at), per global-constraints. node is "" for the master's own alerts.
type alertDedupKey struct {
	node    string
	key     string
	firedAt int64
}

// fleetAlertEngine is the master's alerting engine core: enrich -> dedup -> decide
// local-vs-master delivery -> record -> deliver -> receipt.
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

	// sendResolved gates whether a recover is actually delivered (still always recorded in the
	// incident either way).
	sendResolved bool

	// dispatch runs every deliverAndReceipt call off Submit's own goroutine (see Submit),
	// keyed per (node, key) so a recover is never dispatched before its own fire.
	dispatch *keyedDispatcher

	// silences and nodeInfo are wired once, after construction, via SetSilences: a nil
	// silences (the default) makes every suppression check and TickSilences call a no-op.
	silences        *silenceStore
	nodeInfo        func(nodeID string) (name string, tags []string)
	lastSilencePush int64 // unix time of the last push-to-all pass; 0 means never

	// alerting, deliverNamed and dispatchOnly are wired once, after construction, via
	// SetRouting -- exactly SetSilences's pattern (see its doc comment).
	alerting     *alertingStore
	deliverNamed func(a Alert, channels []string) bool
	dispatchOnly func(a Alert, channels []string) bool

	// depsOf, wired once via SetDependencies, returns the EXPANDED list of node ids a given
	// node id depends on (any "tag:<t>" entry already resolved against the current registry).
	depsOf func(nodeID string) []string

	// getCfg, wired once via SetConfig, reads the master's LIVE config -- specifically
	// fleet.fallback_after.
	getCfg func() *config.Config

	// rules, wired once via SetRules, is the aggregate-rule evaluator TickRules drives every
	// ruleTickInterval.
	rules *fleetRuleEvaluator
}

// newFleetAlertEngine builds a fleetAlertEngine. now defaults to time.Now if nil.
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

// ruleFromKey returns key's "rule" component for the default (rule, severity) group key.
func ruleFromKey(key string) string { return key }

// groupKeyFor computes the grouping bucket an alert with the given source/key/severity
// joins: an open incident whose OWN GroupKey equals this exact string is joined.
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

// groupKeyForRouted resolves src/a's matched route (exactly like real delivery --
// resolveRoute) to find its GroupBy override, if any, then computes groupKeyFor.
func (e *fleetAlertEngine) groupKeyForRouted(src alertSource, a Alert) string {
	var cfg core.AlertingConfig
	if e.alerting != nil {
		cfg = e.alerting.Get()
	}
	res := resolveRoute(cfg, src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())
	return groupKeyFor(res.GroupBy, src, a)
}

// nodeDownKey returns the node-down alert key for nodeID -- the one place this exact format
// is built, matched by masterLoop.stillActive/ checkOrphanedIncidents.
func nodeDownKey(nodeID string) string { return "fleet:node:" + nodeID + ":down" }

// dependencyFoldReason reports whether a node-down FIRE for nodeID should be folded into a
// dependency's open node-down incident instead of delivered on its own.
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
		// SevCritical is hardcoded here: every node-down alert this codebase ever fires uses it
		// (fleetAlert, fleet_daemon.go), and the default group bucket depends on severity.
		gk := groupKeyFor(nil, alertSource{}, Alert{Key: key, Severity: SevCritical})
		return "suppressed: parent " + name + " down", gk, true
	}
	return "", "", false
}

// releaseFoldedDependents runs after a node-down RECOVER is applied: every
// folded (Suppressed != "", unresolved) member across all open incidents is
// re-checked against dependencyFoldReason, and if NONE of its dependencies are
// down any more it is released and delivered as its own new incident.
// recoveredNodeID only feeds the release reason text; the "still down?" check
// re-derives from current incident state, so a member with MULTIPLE down
// dependencies stays folded until every one clears.
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

// ReleaseIfDependenciesClear re-checks whether nodeID's node-down alert, if folded into a
// dependency's incident, can be released given nodeID's CURRENT DependsOn list.
func (e *fleetAlertEngine) ReleaseIfDependenciesClear(nodeID string, now int64) {
	if e.incidents == nil || e.depsOf == nil {
		return
	}
	key := nodeDownKey(nodeID)
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		for _, al := range inc.Alerts {
			if al.Node != "" || al.Key != key || al.ResolvedAt != 0 || al.Suppressed == "" {
				continue
			}
			if _, _, stillFolded := e.dependencyFoldReason(nodeID); stillFolded {
				return // still has a down dependency: leave it folded.
			}
			reason := "released: dependency change"
			_, released, err := e.incidents.ReleaseFoldedMember(inc.ID, al.Node, al.Key, al.FiredAt, now, reason)
			if err != nil || !released {
				return
			}
			e.deliverReleasedMember(al, now)
			return
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
// Ordering: the decision is durably recorded via
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
// dispatcher: Enqueue itself never
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
		// We already made the fire/recover decision for this exact (node, key, fired_at) once.
		if deliveredLocally && e.incidents != nil {
			_, _, _ = e.incidents.MarkDeliveredLocally(src.NodeID, a.Key, firedAt, e.now().Unix())
		}
		return
	}

	// A silence/maintenance window check only matters when this alert would otherwise actually
	// be delivered by the master.
	var supp *suppressionInfo
	if !deliveredLocally && e.silences != nil {
		supp = e.silences.Suppressed(e.now().Unix(), src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())
	}

	// A node-down FIRE is folded into a down dependency's own incident, silently, instead of
	// grouped/delivered on its own -- checked only for an otherwise-deliverable fire.
	var depFold, depGroupKey string
	if a.Kind != "recover" && !deliveredLocally && supp == nil {
		if key, ok := nodeDownTarget(a.Key); ok {
			if reason, gk, folded := e.dependencyFoldReason(key); folded {
				depFold, depGroupKey = reason, gk
			}
		}
	}

	// The grouping bucket this alert joins/opens: a dependency fold always targets its
	// parent's own incident's bucket directly (see dependencyFoldReason).
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

	// A node-down RECOVER may free up other nodes that were folded into it.
	if a.Kind == "recover" {
		if recoveredID, ok := nodeDownTarget(a.Key); ok {
			e.releaseFoldedDependents(recoveredID, e.now().Unix())
		}
	}

	if deliveredLocally {
		return // the child already delivered this; nothing more to do.
	}

	if supp != nil {
		// Suppressed: recorded above, never dispatched.
		if src.NodeID != "" && e.push != nil {
			e.pushReceipt(src.NodeID, a.Key, firedAt)
		}
		return
	}

	// The dispatcher lane key is the incident id, so every job touching this
	// incident (this fire/recover, another member's, a group notification, an
	// escalation) runs strictly in enqueue order. incidentID is "" only when
	// e.incidents is nil (no store, e.g. a bare-bones test): incidentGroupKey is
	// then a stable per-(node,key) lane so ordering still holds for that pair.
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

// deliverAndReceipt is Submit's steps 2-4, run off Submit's own goroutine (via the keyed
// dispatcher): attempt delivery.
func (e *fleetAlertEngine) deliverAndReceipt(src alertSource, a Alert, firedAt int64, incidentID string) {
	e.deliverAndReceiptDetail(src, a, firedAt, incidentID, "sent via the master's dispatcher")
}

// handleRecover is a recover leg's own dispatch job: an incident with a still-open OTHER
// member is not over yet, so this member's own recover gets no human-facing notification.
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

// fleetFallbackAfterDefault mirrors config.Default()'s own fleet.fallback_after
// (2m) for effectiveGroupInterval's use when the engine has no getCfg wired
// (SetConfig never called, e.g. every pre-existing engine test) --
// newMasterLoop's own "no getCfg" fallback follows the identical pattern for
// fleet.node_down_after.
const fleetFallbackAfterDefault = 2 * time.Minute

// effectiveGroupInterval is tryDeliverGroup's update cadence: groupInterval, UNLESS pending
// holds a child-sourced member (Node != "").
func (e *fleetAlertEngine) effectiveGroupInterval(pending []core.IncidentAlert) time.Duration {
	hasChildPending := false
	for _, al := range pending {
		if al.Node != "" {
			hasChildPending = true
			break
		}
	}
	if !hasChildPending {
		return groupInterval
	}
	fallback := fleetFallbackAfterDefault
	if e.getCfg != nil {
		if c := e.getCfg(); c != nil {
			fallback = c.FleetFallbackAfter()
		}
	}
	if half := fallback / 2; half < groupInterval {
		return half
	}
	return groupInterval
}

// tryDeliverGroup runs INSIDE id's own keyed lane (enqueued by Submit for every fire, and
// by TickGrouping's periodic re-check): the master's incident-grouping delivery.
func (e *fleetAlertEngine) tryDeliverGroup(id string) {
	if id == "" || e.incidents == nil {
		return
	}
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State == "suppressed" {
		return // every open member silenced or folded: never group-delivered.
	}
	// pending is deliberately NOT filtered by ResolvedAt: Apply records a recover
	// SYNCHRONOUSLY in the caller's goroutine when Submit is called.
	var pending []core.IncidentAlert
	anyDelivered := false
	for _, al := range inc.Alerts {
		if al.Suppressed != "" || al.SilencedBy != "" {
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
	} else if last := lastGroupDeliveryTS(inc); now.Before(time.Unix(last, 0).Add(e.effectiveGroupInterval(pending))) {
		return // an update was sent too recently; the next tick will catch this.
	}
	e.deliverGroup(inc, pending, now.Unix())
}

// lastGroupDeliveryTS returns the most recent timestamp among every member's fire-leg
// "delivered" event.
func lastGroupDeliveryTS(inc core.Incident) int64 {
	var ts int64
	for _, ev := range inc.Timeline {
		if ev.Kind == "delivered" && ev.Leg == "fire" && ev.TS > ts {
			ts = ev.TS
		}
	}
	return ts
}

// memberNodeName returns al's node display name: al.NodeName as recorded AT
// FIRE TIME (the name Submit's caller passed in), falling back to e.nodeInfo
// (a CURRENT registry lookup, for a legacy IncidentAlert with no NodeName) and
// finally the bare node id.
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

// groupAlertTitle builds the notification title for pending's members: a lone member keeps
// the "<node>: <title>" shape (or the bare title for a master-own alert).
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

// deliverGroup is tryDeliverGroup's actual send: ONE physical delivery covering every
// member in pending and, only on success.
func (e *fleetAlertEngine) deliverGroup(inc core.Incident, pending []core.IncidentAlert, now int64) {
	sev, err := ParseSeverity(inc.Severity)
	if err != nil {
		sev = SevWarning
	}
	title := e.groupAlertTitle(inc, pending)
	lead := pending[0]
	da := Alert{Key: lead.Key, Title: title, Severity: sev, Kind: "fire", Time: now, Buttons: incidentButtons(inc.ID)}

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

// deliverAndReceiptDetail is deliverAndReceipt with a caller-chosen note on the "delivered"
// timeline event.
func (e *fleetAlertEngine) deliverAndReceiptDetail(src alertSource, a Alert, firedAt int64, incidentID, note string) {
	da := a
	if src.NodeID != "" {
		da.Title = src.NodeName + ": " + a.Title
	}
	if a.Kind == "fire" && incidentID != "" {
		// only an incident's fire notification carries Ack/Silence buttons; a recover never does.
		da.Buttons = incidentButtons(incidentID)
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

	// Routing/escalation: resolve the SAME way RouteTest does (resolveRoute) -- EVERY matched
	// policy applies independently (see routeResolution's doc comment).
	if e.deliverNamed == nil {
		return
	}
	cfg := e.alerting.Get()
	res := resolveRoute(cfg, src.NodeID, src.NodeName, src.Tags, a.Key, a.Severity.String())

	if a.Kind == "recover" {
		e.deliverResolved(src, da, firedAt, incidentID, res.Policies)
		return
	}

	// fire: one physical dispatch to the union of every matched policy's step 0, but one
	// timeline event PER policy.
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

// deliverResolved delivers the recover leg: the resolved message goes to the
// union of every channel that received a step (fire, escalated or repeated)
// from a policy whose SendResolved is true (default true). If history has
// nothing to derive that from (e.g. a resurrection edge case), it falls back
// to the union of step 0 across every SendResolved-true matched policy, so a
// legitimate resolved message is never dropped. If NO matched policy wants it,
// nothing is delivered.
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

// policyStepDetail formats a step-N delivery/escalation/repeat timeline event's Detail:
// "policy P step N: chan1, chan2" -- used for every routed per-policy delivery.
func policyStepDetail(policy string, step int, channels []string) string {
	return fmt.Sprintf("policy %s step %d: %s", policy, step, strings.Join(channels, ", "))
}

// stepEventDetailRe/legacyStepDetailRe parse policyStepDetail's format and its
// legacy predecessor ("step N: chan1, chan2", no policy name; legacy events
// belong to the first matched policy). A policy name is taken greedily up to
// the LAST " step N: ", so a name containing " step " still parses.
var (
	stepEventDetailRe  = regexp.MustCompile(`^policy (.+) step (\d+): (.*)$`)
	legacyStepDetailRe = regexp.MustCompile(`^step (\d+): (.*)$`)
)

// stepEventInfo extracts (policy, step, channels) from a "delivered" (fire leg only),
// "escalated" or "repeated" timeline event.
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
// every channel from a step-event (fire step 0, any escalation or repeat)
// whose OWNING POLICY has SendResolved true; a policy no longer in the current
// matched set (config changed mid-incident) defaults to true.
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

// legDeliveredStatus scans inc's timeline for "delivered" events and reports which leg(s)
// (fire, recover) they cover, per deliverAndReceiptDetail's "fire: "/"recover: " labeling.
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

// legDeliveredStatusFor is legDeliveredStatus scoped to ONE member alert, matched by (Node,
// AlertKey, FiredAt).
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

// resurrectMasterAlerts runs once, at engine construction: master-own alerts
// (Node == "" on the IncidentAlert, e.g. node-down/connectivity) have no
// child-side fallback if the master crashes between recording a fire/recover
// (Submit's step 1) and delivering it, so nothing else will retry it.
//
// It checks EVERY master-own member of EVERY incident independently (a grouped
// or folded incident can hold several with independent delivery histories),
// skipping a dependency-suppressed one (Suppressed != "": deliberately never
// delivered on its own). For each remaining member the FIRE and RECOVER legs
// are checked independently (a single check on the incident's last entry could
// miss an undelivered fire when the later recover looks undelivered): for a
// member younger than 24h, any leg with no matching "delivered" event
// (legDeliveredStatusFor) is re-enqueued on the incident's keyed lane, fire
// before recover, as an ordinary Submit would. At 24h or older it gives up on
// the undelivered leg(s) and records why once, without delivering; that event
// deliberately carries no Leg so it is never mistaken for "delivered".
func (e *fleetAlertEngine) resurrectMasterAlerts() {
	if e.incidents == nil {
		return
	}
	now := e.now()
	cutoff := now.Add(-24 * time.Hour).Unix()
	for _, inc := range e.incidents.List(core.IncidentFilter{}, nil) {
		id := inc.ID
		// Every member whose FIRE needs resurrecting is batched into ONE tryDeliverGroup job on
		// the incident's lane, rather than one deliverAndReceiptDetail call per member.
		needsGroupDelivery := false
		var pendingRecovers []core.IncidentAlert
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

			if !fireDelivered {
				needsGroupDelivery = true
			}
			if hasRecover && !recoverDelivered {
				pendingRecovers = append(pendingRecovers, al)
			}
		}
		// Pass 2: enqueue, group-fire first.
		if needsGroupDelivery {
			e.dispatch.Enqueue(id, func() { e.tryDeliverGroup(id) })
		}
		for _, al := range pendingRecovers {
			a := Alert{Key: al.Key, Title: al.Title, Severity: mustSeverity(al.Severity), Kind: "recover", Time: al.ResolvedAt}
			firedAt := al.ResolvedAt
			e.dispatch.Enqueue(id, func() {
				e.deliverAndReceiptDetail(alertSource{}, a, firedAt, id, "redelivered after restart")
			})
		}
	}
}

// waitIdleForTest blocks until every deliverAndReceipt job Submit has enqueued so far has
// actually run.
func (e *fleetAlertEngine) waitIdleForTest() { e.dispatch.waitIdleForTest() }

// Stop stops the engine's keyed dispatcher accepting new work and waits up to timeout for
// everything already queued or in flight to finish.
func (e *fleetAlertEngine) Stop(timeout time.Duration) bool { return e.dispatch.Stop(timeout) }

// HandleChildAlert converts a child's shipped AlertEvent (as decoded by the replica from a
// KindAlert record) into an Alert and submits it, using FiredAt.
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

// TickLeases pushes a fresh lease to every connected node in ids whose last lease push was
// leaseInterval or more ago (or never pushed).
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

// PushLeaseNow pushes a lease to id unconditionally, bypassing the 30s cadence gate -- used
// on Hub.OnConnect, so a freshly.
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

// HandleChildAckSync is the master's entry point for a child-side ack/unack reaching it: it
// is called whenever a node's alerts.json actually changed (replicaSink.Live -> onAckSync).
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
		// Looked up by the specific (node, key) MEMBER, not the incident's own grouping bucket: a
		// grouped incident's bucket key no longer has anything to do with (node, key) by default.
		inc, ok := e.incidents.OpenAlertIncident(nodeID, key)
		if !ok || inc.State == "acked" || inc.State == "resolved" {
			continue
		}
		_, _ = e.incidents.Ack(inc.ID, "node:"+nodeID, now)
	}
}

// SetSilences wires the engine to the master's silence/maintenance store and a node info
// lookup (name, tags), used by Submit's suppression check.
func (e *fleetAlertEngine) SetSilences(store *silenceStore, nodeInfo func(id string) (string, []string)) {
	e.silences = store
	e.nodeInfo = nodeInfo
}

// SetRouting wires the engine to the master's routing/escalation config store and its
// channel-scoped delivery functions, mirroring SetSilences's pattern.
func (e *fleetAlertEngine) SetRouting(store *alertingStore, deliverNamed, dispatchOnly func(a Alert, channels []string) bool) {
	e.alerting = store
	e.deliverNamed = deliverNamed
	e.dispatchOnly = dispatchOnly
}

// SetDependencies wires the engine to a node-dependency resolver.
func (e *fleetAlertEngine) SetDependencies(depsOf func(nodeID string) []string) {
	e.depsOf = depsOf
}

// SetConfig wires the engine to the master's live config, called once from startMaster
// after the engine exists.
func (e *fleetAlertEngine) SetConfig(getCfg func() *config.Config) {
	e.getCfg = getCfg
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

// deliverUnsilenced scans the incidents indexed as suppressed-and-open
// (incidentStore.SuppressedOpen, a maintained index rather than a full List()
// scan every 5s) and, for each whose most recent alert has no ResolvedAt,
// enqueues a RE-CHECK job on that incident's keyed lane. The "still
// suppressed, unresolved, silenced?" decision is made inside that job
// (tryDeliverUnsilenced), not here.
func (e *fleetAlertEngine) deliverUnsilenced(now time.Time) {
	if e.incidents == nil || e.silences == nil {
		return
	}
	for _, inc := range e.incidents.SuppressedOpen() {
		id := inc.ID
		e.dispatch.Enqueue(id, func() { e.tryDeliverUnsilenced(id) })
	}
}

// tryDeliverUnsilenced runs INSIDE id's keyed lane (enqueued by deliverUnsilenced): by the
// time it executes, other jobs for the same incident.
func (e *fleetAlertEngine) tryDeliverUnsilenced(id string) {
	inc, ok := e.incidents.Get(id)
	if !ok {
		return
	}
	for _, al := range inc.Alerts {
		if al.ResolvedAt != 0 || al.SilencedBy == "" {
			continue // recovered, or not (or no longer) silenced
		}
		name, tags := al.Node, []string(nil)
		if al.Node != "" && e.nodeInfo != nil {
			name, tags = e.nodeInfo(al.Node)
		}
		if e.silences.Suppressed(e.now().Unix(), al.Node, name, tags, al.Key, al.Severity) != nil {
			continue // silenced again (or still) by the time this job actually ran
		}
		e.deliverUnsilencedMember(id, al, name, tags)
	}
}

// deliverUnsilencedMember delivers al -- and ONLY al, not the rest of its incident -- once
// its silence has lifted.
func (e *fleetAlertEngine) deliverUnsilencedMember(id string, al core.IncidentAlert, name string, tags []string) {
	a := alertOf(al)
	if al.Node != "" {
		a.Title = e.memberNodeName(al) + ": " + a.Title
	}
	now := e.now().Unix()
	var delivered bool
	var events []core.IncidentEvent
	if e.alerting != nil && e.deliverNamed != nil {
		cfg := e.alerting.Get()
		res := resolveRoute(cfg, al.Node, name, tags, al.Key, al.Severity)
		channels := unionStepChannels(res.Policies, 0)
		delivered = e.deliverNamed(a, channels)
		if delivered {
			for _, p := range res.Policies {
				if len(p.Steps) == 0 {
					continue
				}
				events = append(events, core.IncidentEvent{
					TS: now, Kind: "delivered", Detail: "fire: " + policyStepDetail(p.Name, 0, p.Steps[0].Channels), Actor: "system",
					Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
					Policy: p.Name, Step: 0, Channels: append([]string(nil), p.Steps[0].Channels...),
				})
			}
		}
	} else if e.deliver != nil {
		delivered = e.deliver(a)
		if delivered {
			events = append(events, core.IncidentEvent{
				TS: now, Kind: "delivered", Detail: "fire: delivered after silence ended", Actor: "system",
				Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
			})
		}
	}
	if !delivered {
		return // no channel accepted it: SilencedBy stays set, tried again next sweep.
	}
	_, _, _ = e.incidents.DeliverUnsilencedMember(id, al.Node, al.Key, al.FiredAt, events...)
}

// PushSilencesToAll immediately refreshes every id's pushed silence set (skipping any not
// currently connected), bypassing TickSilences's periodic cadence gate.
func (e *fleetAlertEngine) PushSilencesToAll(now time.Time, ids []string) {
	for _, id := range ids {
		if e.connected != nil && !e.connected(id) {
			continue
		}
		e.pushSilencesNow(id, now)
	}
}

// PushSilencesNow pushes id's current filtered silence set unconditionally -- used on
// Hub.OnConnect, mirroring PushLeaseNow, so a freshly.
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

// TickEscalations evaluates every currently firing (state=="firing" -- not acked,
// suppressed or resolved) incident's escalation schedule.
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

// TickGrouping re-checks every open (firing or acked) incident's pending group
// notification: a fire always tries tryDeliverGroup itself immediately (Submit).
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

// escalatedTo reports whether inc's timeline already records (policy, step) as escalated.
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

// firstFireDeliveryTS returns the timestamp of inc's fire leg's step-0 "delivered" event,
// and whether one exists.
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

// tryEscalate runs INSIDE id's keyed lane (enqueued by TickEscalations): it re-reads the
// incident fresh, and if still firing with its fire leg delivered.
func (e *fleetAlertEngine) tryEscalate(id string, cfg core.AlertingConfig, now time.Time) {
	inc, ok := e.incidents.Get(id)
	if !ok || inc.State != "firing" || len(inc.Alerts) == 0 {
		return
	}
	// A grouped incident's escalation is driven by its most recently fired member that is
	// still active (unresolved and not dependency-suppressed).
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

// escalateStep delivers channels for (policy, step) through the keyed dispatch
// (dispatchOnly: an escalation is not a new alert, so no alert-log/live-bus
// record) and, only on success, durably records {Kind:"escalated",
// Detail:"policy P step N: chan1, chan2"} so a restarted master never resends
// it (escalatedTo).
func (e *fleetAlertEngine) escalateStep(id string, al core.IncidentAlert, policy string, step int, channels []string, now time.Time) bool {
	if e.dispatchOnly == nil {
		return false
	}
	a := alertOf(al)
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
// inc.State=="firing"). Recorded as {Kind:"repeated"}; each
// matched policy is considered independently, with its own
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
	a := alertOf(al)
	if !e.dispatchOnly(a, channels) {
		return
	}
	_, _ = e.incidents.AppendEvent(id, core.IncidentEvent{
		TS: now.Unix(), Kind: "repeated", Detail: policyStepDetail(p.Name, lastReached, channels), Actor: "system",
		Leg: "fire", AlertKey: al.Key, Node: al.Node, FiredAt: al.FiredAt,
		Policy: p.Name, Step: lastReached, Channels: append([]string(nil), channels...),
	})
}

// alertOf rebuilds the ordinary "fire" Alert an escalation/repeat/unsilence notification
// re-sends from a recorded IncidentAlert.
func alertOf(al core.IncidentAlert) Alert {
	return Alert{Key: al.Key, Title: al.Title, Severity: mustSeverity(al.Severity), Kind: "fire", Time: al.FiredAt}
}

// latestActiveAlert returns inc's most recently fired member alert that is still active --
// unresolved, not dependency-suppressed, and not silenced.
func latestActiveAlert(inc core.Incident) (core.IncidentAlert, bool) {
	var best core.IncidentAlert
	found := false
	for _, a := range inc.Alerts {
		if a.ResolvedAt != 0 || a.Suppressed != "" || a.SilencedBy != "" {
			continue
		}
		if !found || a.FiredAt > best.FiredAt {
			best, found = a, true
		}
	}
	return best, found
}

// PushAck pushes an "ack" (unack=false) or "unack" (unack=true) frame for key to nodeID,
// applied on the child via AlertState.Ack/Unack (see applyAckFrame in fleet_lease.go).
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
