// Package trinetra: fleet_lease.go implements the child side of the
// master's lease-based alert handoff (spec 4.8/6): while the master holds a
// delivery lease, a firing (or recovering) alert is routed to it instead of
// delivered locally, and only falls back to local delivery if the master's
// receipt never arrives, or the lease itself expires, before
// fleet.fallback_after.
package trinetra

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
// pending, so Tick will never fall it back to local delivery. It reports the
// Kind ("fire" or "recover") of the pending entry it resolved and whether
// one was actually found, so a caller (onStreamFrame) can durably record
// which kind of receipt this was -- the receipt frame itself carries no
// Kind (wire format is just {"key","fired_at"}), only whatever Route/
// Reconcile already stored for that (key, firedAt).
//
// A receipt for something not (or no longer) pending -- late, duplicate, or
// for an alert this process never routed -- is a harmless no-op: ok is
// false and kind is "".
func (h *handoff) Receipt(key string, firedAt int64) (kind string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := handoffKey{key, firedAt}
	p, exists := h.pending[k]
	if !exists {
		return "", false
	}
	delete(h.pending, k)
	return p.alert.Kind, true
}

// Reconcile re-adds alerts a restarted process lost from memory (see
// reconcilePendingFromLog), preserving each one's OWN Time as its routedAt,
// so Tick judges it exactly as if Route had been called back when it
// actually fired -- an alert that fired long enough ago is delivered on the
// very next Tick, not after a fresh fallbackAfter countdown starting now.
//
// An entry already pending (Route ran again for the same (key, firedAt)
// during the brief window between startChild constructing handoff and
// calling Reconcile -- possible if a lease frame and a fresh fire race the
// reconciliation scan) is left alone rather than overwritten, since Route's
// own routedAt is at least as accurate as the log's.
func (h *handoff) Reconcile(alerts []Alert) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range alerts {
		key := handoffKey{a.Key, a.Time}
		if _, exists := h.pending[key]; !exists {
			h.pending[key] = pendingAlert{alert: a, routedAt: a.Time}
		}
	}
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

// handoffReceipt is one line in the child-private receipts sidecar (see
// handoffReceiptsPath): a durable record that the master acknowledged
// (Key, FiredAt) of the given Kind, so a restarted child's reconciliation
// (reconcilePendingFromLog) knows not to treat it as still pending.
//
// Deliberately NOT part of alertlog.jsonl: that file feeds every unfiltered
// alert-history reader (alertHistoryRecords -> the web UI's /alerts history,
// `trinetra alerts list`) and is teed to the outbox (shipped to the master's
// replica, and re-shipped by localGapFiller on gap repair) -- a receipt is
// neither a fire nor a recover a human or the master's replica should ever
// see as an alert. It is purely this child's own internal handoff
// bookkeeping, so it gets its own small sidecar file instead.
type handoffReceipt struct {
	Key     string `json:"key"`
	FiredAt int64  `json:"fired_at"`
	Kind    string `json:"kind"` // "fire" | "recover"
	TS      int64  `json:"ts"`   // when this receipt was recorded
}

// handoffReceiptsPath is the sidecar's path under the daemon's state
// directory: <stateDir>/fleet-child/handoff-receipts.jsonl. fleet-child/
// already exists by the time this is ever used (fleet.LoadIdentity requires
// it, and startChild calls that before anything here runs).
func handoffReceiptsPath(stateDir string) string {
	return filepath.Join(fleetChildDir(stateDir), "handoff-receipts.jsonl")
}

// appendHandoffReceipt durably (fsync'd) appends one receipt line to path.
func appendHandoffReceipt(path string, r handoffReceipt) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readHandoffReceipts returns every receipt in path with TS >= sinceUnix,
// mirroring AlertLog.AlertEventsSince: a missing file is not an error (no
// receipts recorded yet), and a corrupt/malformed line is skipped rather
// than failing the whole read.
func readHandoffReceipts(path string, sinceUnix int64) ([]handoffReceipt, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []handoffReceipt
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r handoffReceipt
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.TS >= sinceUnix {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// pruneHandoffReceipts rewrites path keeping only receipts with TS >=
// beforeUnix, mirroring AlertLog.PruneAlertLog. Called once at startChild
// (the same reconcile window used by reconcilePendingFromLog) so the
// sidecar does not grow forever; a missing file is a no-op.
func pruneHandoffReceipts(path string, beforeUnix int64) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	rs, err := readHandoffReceipts(path, beforeUnix)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return writeFileAtomic(path, buf.Bytes(), 0o600)
}

// onStreamFrame is startChild's fleet.ShipperConfig.OnFrame: it decodes a
// "lease" or "receipt" frame and applies it to lease/h, ignoring anything
// else (silences, managed_config, rpc, ack/unack are other tasks' frame
// types; ping never reaches OnFrame at all -- the stream client filters it
// out itself). A malformed payload is dropped rather than panicking: OnFrame
// runs synchronously on the stream's read loop, so it must never block or
// crash on hostile/garbled input from the wire.
//
// A "receipt" frame that actually resolved something pending is also
// durably recorded in the receipts sidecar at receiptsPath (see
// handoffReceipt) so a restart between this receipt and the alert's
// eventual resolution doesn't resurrect it as pending
// (reconcilePendingFromLog). now is injected (rather than calling time.Now
// directly) purely so a test can pin the recorded TS without sleeping;
// production passes time.Now.
//
// Every branch here is cheap (a JSON unmarshal of a tiny fixed struct, a
// mutex-guarded field/map update, and -- only for a receipt that actually
// resolved something -- one small appended, fsync'd line) so this
// comfortably meets OnFrame's "must not block" contract (see
// ShipperConfig.OnFrame's doc comment); that fsync cost is the same one
// every ordinary alert already pays on this same disk via AlertLog.
func onStreamFrame(lease *leaseHolder, h *handoff, receiptsPath string, now func() time.Time, f fleet.Frame) {
	switch f.Type {
	case "lease":
		var p leaseFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			lease.Grant(p.Until)
		}
	case "receipt":
		var p receiptFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			if kind, ok := h.Receipt(p.Key, p.FiredAt); ok {
				_ = appendHandoffReceipt(receiptsPath, handoffReceipt{
					Key: p.Key, FiredAt: p.FiredAt, Kind: kind, TS: now().Unix(),
				})
			}
		}
	}
}

// reconcilePendingFromLog rebuilds the routed alerts (fires AND recovers) a
// restarted child lost from memory (handoff.pending is in-memory only): any
// "fire" or "recover" AlertEvent logged as RoutedToMaster that has no later
// entry resolving the same (key, fired_at) -- a receipt in the sidecar at
// receiptsPath, a delivered_locally record, or (fires only) a recover for
// that key logged after it -- is still owed either a receipt or a fallback
// delivery, and is returned here for handoff.Reconcile to re-add (with its
// original fire/recover time preserved, so Tick's fallback timing applies as
// if the process never restarted). The "never zero times" rule (see
// global-constraints) covers recovers exactly like fires, so both are
// reconciled the same way.
//
// The scan is bounded to the last fallbackAfter*10 window (generous enough
// that an event right at the edge of a normal fallback window is never
// missed, short enough that a long-lived, mostly-pruned log cannot slow
// startup) rather than the whole file, for both alertlog.jsonl and the
// receipts sidecar.
func reconcilePendingFromLog(alog *AlertLog, receiptsPath string, fallbackAfter time.Duration, now time.Time) []Alert {
	if alog == nil {
		return nil
	}
	since := now.Add(-10 * fallbackAfter).Unix()
	events, err := alog.AlertEventsSince(since)
	if err != nil {
		return nil
	}
	// A read failure here just means fewer resolutions are found than truly
	// happened -- an unresolved entry gets redelivered instead of lost,
	// which is the safe direction per "never zero times" (a redundant
	// redelivery is the documented fallback race, not a bug).
	receipts, _ := readHandoffReceipts(receiptsPath, since)

	pending := map[handoffKey]Alert{}
	for _, ev := range events {
		key := handoffKey{ev.Key, ev.FiredAt}
		if (ev.Kind == "fire" || ev.Kind == "recover") && ev.RoutedToMaster {
			sev, err := ParseSeverity(ev.Severity)
			if err != nil {
				sev = SevWarning // never drop the alert over an unparsable severity
			}
			pending[key] = Alert{
				Key:      ev.Key,
				Title:    ev.Title,
				Severity: sev,
				Kind:     ev.Kind,
				Source:   ev.Source,
				Time:     ev.FiredAt,
			}
		}
		if ev.DeliveredLocally {
			delete(pending, key)
		}
		if ev.Kind == "recover" {
			// ANY recover for this key -- routed or not, receipted or not --
			// resolves an earlier pending FIRE for the same key: the
			// condition already cleared, so there is no point still chasing
			// a stale fire notification. This does not touch a pending
			// RECOVER (only a fire): recovers don't resolve each other.
			for k, a := range pending {
				if k.key == ev.Key && a.Kind == "fire" && ev.Time > k.firedAt {
					delete(pending, k)
				}
			}
		}
	}
	for _, r := range receipts {
		delete(pending, handoffKey{r.Key, r.FiredAt})
	}

	out := make([]Alert, 0, len(pending))
	for _, a := range pending {
		out = append(out, a)
	}
	return out
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
