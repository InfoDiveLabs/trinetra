// Package trinetra: fleet_lease.go implements the child side of the master's lease-based
// alert handoff: while the master holds a delivery lease, a firing.
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

// fallbackPrefix is prepended, verbatim, to a fallback-delivered alert's title, so a human
// can tell "the master delivered this" apart from "the master never got the chance to."
const fallbackPrefix = "via local fallback: master unreachable — "

// revokedFallbackPrefix is used instead of fallbackPrefix when draining handoff.pending
// because the node was revoked (handleLinkRevocation).
const revokedFallbackPrefix = "node revoked, delivering locally — "

// leaseMaxDuration caps how far into the future a granted lease may reach, regardless of
// what the master's lease frame claims.
const leaseMaxDuration = 120 * time.Second

// leaseHolder tracks whether the master currently holds a delivery lease,
// judged by this child's own clock.
type leaseHolder struct {
	now func() time.Time

	mu    sync.Mutex
	until int64 // unix seconds; 0 before any lease has ever been granted
}

// newLeaseHolder builds a leaseHolder with no lease held.
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

// Revoke marks the lease permanently invalid: Valid() returns false from this call onward.
func (l *leaseHolder) Revoke() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.until = 0
}

// handoffKey identifies one routed alert the same way the master's dedup does.
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

// handoff is the child-side state machine deciding, per alert, whether it is delivered
// locally right now (Route) or held pending a receipt from the master.
type handoff struct {
	now           func() time.Time
	fallbackAfter func() time.Duration
	lease         *leaseHolder

	mu      sync.Mutex
	pending map[handoffKey]pendingAlert
}

// newHandoff builds a handoff. now defaults to time.Now if nil. fallbackAfter is read fresh
// on every Tick, not captured once, so fleet.fallback_after's live-apply.
func newHandoff(now func() time.Time, fallbackAfter func() time.Duration, lease *leaseHolder) *handoff {
	if now == nil {
		now = time.Now
	}
	return &handoff{now: now, fallbackAfter: fallbackAfter, lease: lease, pending: map[handoffKey]pendingAlert{}}
}

// Route decides whether a is delivered locally right now.
func (h *handoff) Route(a Alert) bool {
	if h.lease == nil || !h.lease.Valid() {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending[handoffKey{a.Key, a.Time}] = pendingAlert{alert: a, routedAt: h.now().Unix()}
	return false
}

// Receipt marks (key, firedAt) as delivered by the master: dropped from pending, so Tick
// will never fall it back to local delivery.
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
// reconcilePendingFromLog).
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

// Tick returns every pending alert now overdue for local delivery -- its receipt wait has
// exceeded fallbackAfter(), or the lease has expired outright.
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

// Drain unconditionally clears and returns every pending alert, regardless of
// fallbackAfter/lease timing -- unlike Tick.
func (h *handoff) Drain() []Alert {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.pending) == 0 {
		return nil
	}
	out := make([]Alert, 0, len(h.pending))
	for k, p := range h.pending {
		out = append(out, p.alert)
		delete(h.pending, k)
	}
	return out
}

// leaseFrameData and receiptFrameData are the exact JSON payload shapes carried in a
// "lease"/"receipt" stream Frame's Data, per the fleet phase-2 spec: lease {"until".
type leaseFrameData struct {
	Until int64 `json:"until"`
}
type receiptFrameData struct {
	Key     string `json:"key"`
	FiredAt int64  `json:"fired_at"`
}

// ackFrameData is the "ack"/"unack" stream frame's Data shape: the master's AckIncident
// (fleet_engine.go) pushes one of these per member node.
type ackFrameData struct {
	Key string `json:"key"`
}

// handoffReceipt is one line in the child-private receipts sidecar (see
// handoffReceiptsPath): a durable record that the master acknowledged.
type handoffReceipt struct {
	Key     string `json:"key"`
	FiredAt int64  `json:"fired_at"`
	Kind    string `json:"kind"` // "fire" | "recover"
	TS      int64  `json:"ts"`   // when this receipt was recorded
}

// handoffReceiptsPath is the sidecar's path under the daemon's state directory.
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

// readHandoffReceipts returns every receipt in path with TS >= sinceUnix, mirroring
// AlertLog.AlertEventsSince: a missing file is not an error (no receipts recorded yet).
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

// pruneHandoffReceipts rewrites path keeping only receipts with TS >= beforeUnix, mirroring
// AlertLog.PruneAlertLog.
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

// onStreamFrame is startChild's fleet.ShipperConfig.OnFrame: it decodes a "lease",
// "receipt" or "silences" frame and applies it to lease/h/silences, ignoring anything else.
func onStreamFrame(lease *leaseHolder, h *handoff, receiptsPath string, silences *pushedSilences, now func() time.Time, f fleet.Frame) {
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
	case "silences":
		var p silencesFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			_ = silences.Set(p.Silences)
		}
	}
}

// applyAckFrame handles the "ack"/"unack" stream frame types the master's AckIncident
// pushes (fleet_engine.go's PushAck).
func applyAckFrame(self core.API, f fleet.Frame) {
	if self == nil {
		return
	}
	switch f.Type {
	case "ack":
		var p ackFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			_ = self.AckAlert(p.Key)
		}
	case "unack":
		var p ackFrameData
		if json.Unmarshal(f.Data, &p) == nil {
			_ = self.UnackAlert(p.Key)
		}
	}
}

// reconcilePendingFromLog rebuilds the routed alerts (fires AND recovers) a restarted child
// lost from memory (handoff.pending is in-memory only).
func reconcilePendingFromLog(alog *AlertLog, receiptsPath string, fallbackAfter time.Duration, now time.Time) []Alert {
	if alog == nil {
		return nil
	}
	since := now.Add(-10 * fallbackAfter).Unix()
	events, err := alog.AlertEventsSince(since)
	if err != nil {
		return nil
	}
	// A read failure here just means fewer resolutions are found than truly happened -- an
	// unresolved entry gets redelivered instead of lost.
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
			// ANY recover for this key -- routed or not, receipted or not -- resolves an earlier
			// pending FIRE for the same key: the condition already cleared.
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

// deliverFallback is what startChild's handoff ticker calls for every Alert handoff.Tick
// returns: the master's receipt never arrived (or the lease expired) in time.
func deliverFallback(silences *pushedSilences, alog *AlertLog, bus *eventBus, q *NotifierQueue, a Alert, quiet bool, nowUnix int64, prefix string) {
	firedAt := a.Time
	reason, suppressed := silences.Suppressed(nowUnix, a.Key, a.Severity.String())
	if suppressed {
		a.Title = "silenced (" + reason + "): " + a.Title
	} else {
		a.Title = prefix + a.Title
	}
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
	if suppressed {
		return // recorded above (delivered_locally=true), never actually delivered
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
