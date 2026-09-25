// Package trinetra: fleet_lease.go implements the child side of the
// master's lease-based alert handoff (spec 4.8/6): while the master holds a
// delivery lease, a firing (or recovering) alert is routed to it instead of
// delivered locally, and only falls back to local delivery if the master's
// receipt never arrives, or the lease itself expires, before
// fleet.fallback_after.
package trinetra

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// fallbackPrefix is prepended, verbatim, to a fallback-delivered alert's
// title, so a human can tell "the master delivered this" apart from "the
// master never got the chance to."
const fallbackPrefix = "via local fallback: master unreachable — "

// leaseMaxDuration caps how far into the future a granted lease may reach,
// regardless of what the master's lease frame claims. The lease's valid
// duration is the master's call (it sends `until`, a unix time, in every
// lease frame); this side only ever trusts `until` against its OWN clock,
// and clamps it here, so a skewed or misbehaving master can never grant an
// effectively endless lease that would silently swallow every future alert.
const leaseMaxDuration = 120 * time.Second

// leaseHolder tracks whether the master currently holds a delivery lease,
// judged by this child's own clock.
type leaseHolder struct {
	now func() time.Time

	mu    sync.Mutex
	until int64 // unix seconds; 0 before any lease has ever been granted
}

// newLeaseHolder builds a leaseHolder with no lease held. A nil now
// defaults to time.Now.
func newLeaseHolder(now func() time.Time) *leaseHolder {
	if now == nil {
		now = time.Now
	}
	return &leaseHolder{now: now}
}

// Grant records a lease valid until the unix second until (as sent in a
// "lease" stream frame), clamped to at most leaseMaxDuration from now.
func (l *leaseHolder) Grant(until int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cap := l.now().Add(leaseMaxDuration).Unix(); until > cap {
		until = cap
	}
	l.until = until
}

// Valid reports whether a lease is currently held.
func (l *leaseHolder) Valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.until > l.now().Unix()
}

// handoffKey identifies one routed alert the same way the master's
// dedup does (spec: dedup key is node_id, alert_key, fired_at -- node_id is
// implicit here, this side only ever tracks its own): (alert key, the
// fire/recover event's own unix time). A fire and its later recover share
// Key but carry different Time, so they are tracked -- and can
// independently time out -- as separate pending entries.
type handoffKey struct {
	key     string
	firedAt int64
}

// pendingAlert is one alert currently routed to the master: not delivered
// locally, waiting on either a receipt or a fallback decision.
type pendingAlert struct {
	alert    Alert
	routedAt int64 // unix seconds Route() decided not to deliver it locally
}

// handoff is the child-side state machine deciding, per alert, whether it
// is delivered locally right now (Route) or held pending a receipt from the
// master (Route returns false), and falling back to local delivery if that
// receipt never arrives within fallbackAfter(), or the lease expires first
// (Tick).
//
// Route/Receipt/Tick may all be called concurrently: Route from the sampler
// goroutine (via enqueueAndLog -> alertRoute, see daemon.go), Receipt from
// the stream's read loop (via ShipperConfig.OnFrame, which must not block
// -- Receipt is a single guarded map delete, so it never does), Tick from
// startChild's 5s ticker. State is mutex-guarded throughout.
type handoff struct {
	now           func() time.Time
	fallbackAfter func() time.Duration
	lease         *leaseHolder

	mu      sync.Mutex
	pending map[handoffKey]pendingAlert
}

// newHandoff builds a handoff. now defaults to time.Now if nil.
// fallbackAfter is read fresh on every Tick, not captured once, so
// fleet.fallback_after's live-apply (config.go: it is not RestartRequired)
// takes effect immediately, without a restart.
func newHandoff(now func() time.Time, fallbackAfter func() time.Duration, lease *leaseHolder) *handoff {
	if now == nil {
		now = time.Now
	}
	return &handoff{now: now, fallbackAfter: fallbackAfter, lease: lease, pending: map[handoffKey]pendingAlert{}}
}

// Route decides whether a is delivered locally right now. While the master
// holds a valid lease, a is instead recorded as pending -- not delivered
// locally, not lost: Tick eventually either sees a receipt for it (Receipt)
// or falls it back to local delivery -- and Route returns false.
//
// A nil lease (defensive; startChild always supplies one) behaves like an
// always-expired one: Route always returns true. This is also exactly what
// happens for as long as no lease has ever been granted -- an old master
// with no stream endpoint at all, or the window before the first lease
// frame arrives from a new one -- matching today's local-delivery
// behaviour without Route needing to know why no lease exists.
func (h *handoff) Route(a Alert) bool {
	if h.lease == nil || !h.lease.Valid() {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending[handoffKey{a.Key, a.Time}] = pendingAlert{alert: a, routedAt: h.now().Unix()}
	return false
}

// Receipt marks (key, firedAt) as delivered by the master: dropped from
// pending, so Tick will never fall it back to local delivery. A receipt for
// something not (or no longer) pending -- late, duplicate, or for an alert
// this process never routed -- is a harmless no-op.
func (h *handoff) Receipt(key string, firedAt int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.pending, handoffKey{key, firedAt})
}

// Tick returns every pending alert now overdue for local delivery -- its
// receipt wait has exceeded fallbackAfter(), or the lease has expired
// outright -- removing each from pending as it is returned, so it is
// reported at most once. The caller (startChild's 5s ticker) is
// responsible for the actual fallback delivery: prefixing the title,
// enqueuing it locally, and recording the second AlertEvent (see
// deliverFallback).
func (h *handoff) Tick() []Alert {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) == 0 {
		return nil
	}
	now := h.now().Unix()
	leaseExpired := h.lease == nil || !h.lease.Valid()
	fallback := int64(h.fallbackAfter() / time.Second)
	var out []Alert
	for k, p := range h.pending {
		if leaseExpired || now-p.routedAt >= fallback {
			out = append(out, p.alert)
			delete(h.pending, k)
		}
	}
	return out
}

// leaseFrameData and receiptFrameData are the exact JSON payload shapes
// carried in a "lease"/"receipt" stream Frame's Data, per the fleet phase-2
// spec: lease {"until": <unix seconds>}, receipt {"key": "...", "fired_at":
// <unix>}.
type leaseFrameData struct {
	Until int64 `json:"until"`
}
type receiptFrameData struct {
	Key     string `json:"key"`
	FiredAt int64  `json:"fired_at"`
}

// onStreamFrame is startChild's fleet.ShipperConfig.OnFrame: it decodes a
// "lease" or "receipt" frame and applies it to lease/h, ignoring anything
// else (silences, managed_config, rpc, ack/unack are other tasks' frame
// types; ping never reaches OnFrame at all -- the stream client filters it
// out itself). A malformed payload is dropped rather than panicking: OnFrame
// runs synchronously on the stream's read loop, so it must never block or
// crash on hostile/garbled input from the wire.
//
// Both branches are O(1) (a JSON unmarshal of a tiny fixed struct, then a
// single mutex-guarded field/map update) so this comfortably meets OnFrame's
// "must not block" contract (see ShipperConfig.OnFrame's doc comment).
func onStreamFrame(lease *leaseHolder, h *handoff, f fleet.Frame) {
	switch f.Type {
	case "lease":
		var p leaseFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			lease.Grant(p.Until)
		}
	case "receipt":
		var p receiptFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			h.Receipt(p.Key, p.FiredAt)
		}
	}
}

// deliverFallback is what startChild's handoff ticker calls for every Alert
// handoff.Tick returns: the master's receipt never arrived (or the lease
// expired) in time, so it is delivered locally now instead, exactly once.
//
// It deliberately does not go back through enqueueAndLog/alertRoute: a
// still-valid lease with just an overdue receipt would otherwise route it
// right back into pending, and this delivery must be unconditional. Instead
// it mirrors enqueueAndLog's shape directly -- log, publish, enqueue -- with
// two differences: the title carries fallbackPrefix, and the AlertEvent
// records DeliveredLocally with FiredAt equal to the ORIGINAL alert's Time
// (not this delivery's own, later, nowUnix), so the master's dedup key
// (node_id, alert_key, fired_at) still lines up the two records this one
// alert produced -- the earlier "routed_to_master" one Route's caller
// logged, and this one -- as the same alert.
func deliverFallback(alog *AlertLog, bus *eventBus, q *NotifierQueue, a Alert, quiet bool, nowUnix int64) {
	firedAt := a.Time
	a.Title = fallbackPrefix + a.Title
	a.Time = nowUnix
	if alog != nil {
		_ = alog.AppendAlertEvent(AlertEvent{
			Time:             a.Time,
			Key:              a.Key,
			Title:            a.Title,
			Severity:         a.Severity.String(),
			Kind:             a.Kind,
			Source:           a.Source,
			DeliveredLocally: true,
			FiredAt:          firedAt,
		})
	}
	bus.Publish(core.Event{
		Kind:     alertEventKind(a),
		Severity: a.Severity.String(),
		Source:   a.Source,
		Title:    a.Title,
		Time:     a.Time,
	})
	q.Enqueue(a, quiet)
}
