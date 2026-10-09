package trinetra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

func TestLeaseHolderGrantAndValid(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLeaseHolder(func() time.Time { return now })
	if l.Valid() {
		t.Fatal("no lease granted yet, want invalid")
	}
	l.Grant(1090)
	if !l.Valid() {
		t.Fatal("want valid immediately after grant")
	}
	now = time.Unix(1090, 0)
	if l.Valid() {
		t.Fatal("want invalid exactly at until (not < until)")
	}
	now = time.Unix(1089, 0)
	if !l.Valid() {
		t.Fatal("want valid just before until")
	}
}

// A skewed or misbehaving master's far-future lease grant must be clamped to
// leaseMaxDuration from this side's own clock, not trusted verbatim.
func TestLeaseHolderGrantCapsAtMax(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLeaseHolder(func() time.Time { return now })
	l.Grant(1000 + 10_000)
	now = time.Unix(1000+121, 0) // past leaseMaxDuration (120s) from the grant
	if l.Valid() {
		t.Fatal("lease must be capped at leaseMaxDuration from now, not the master's far-future claim")
	}
}

// TestLeaseHolderRevokeInvalidatesImmediately: a valid lease must become (and
// stay) invalid the instant Revoke is called, with no grace window.
func TestLeaseHolderRevokeInvalidatesImmediately(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLeaseHolder(func() time.Time { return now })
	l.Grant(1090)
	if !l.Valid() {
		t.Fatal("want valid before Revoke")
	}
	l.Revoke()
	if l.Valid() {
		t.Fatal("want invalid immediately after Revoke")
	}
}

// TestHandoffDrainReturnsAllPendingRegardlessOfTiming: unlike Tick, handoff.Drain must
// return every pending alert right away.
func TestHandoffDrainReturnsAllPendingRegardlessOfTiming(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000) // stays valid for the whole test
	h := newHandoff(nowFn, func() time.Duration { return time.Hour }, lease)

	a1 := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000}
	a2 := Alert{Key: "mem", Title: "mem high", Kind: "fire", Time: 1000}
	if h.Route(a1) || h.Route(a2) {
		t.Fatal("Route while lease valid must return false")
	}
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick immediately after Route = %v, want none (fallbackAfter is an hour away)", got)
	}

	drained := h.Drain()
	if len(drained) != 2 {
		t.Fatalf("Drain = %v, want both pending alerts returned immediately", drained)
	}
	if len(h.Tick()) != 0 {
		t.Fatal("nothing should remain pending after Drain")
	}
	if got := h.Drain(); len(got) != 0 {
		t.Fatalf("second Drain = %v, want none (idempotent, no double-delivery)", got)
	}
}

// TestHandleLinkRevocationRevokesDrainsOnceAndUsesRevokedPrefix: it must (1) revoke the
// lease.
func TestHandleLinkRevocationRevokesDrainsOnceAndUsesRevokedPrefix(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000)
	h := newHandoff(nowFn, func() time.Duration { return time.Hour }, lease)
	pending := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000}
	if h.Route(pending) {
		t.Fatal("Route while lease valid must return false")
	}

	var delivered []Alert
	var prefixes []string
	fallback := func(a Alert, _ *pushedSilences, prefix string) {
		delivered = append(delivered, a)
		prefixes = append(prefixes, prefix)
	}

	handleLinkRevocation(fleet.LinkStatus{State: fleet.LinkRevoked}, lease, h, nil, fallback)

	if lease.Valid() {
		t.Fatal("want lease invalid after handleLinkRevocation")
	}
	if len(delivered) != 1 || delivered[0].Key != "cpu" {
		t.Fatalf("delivered = %+v, want the one pending alert", delivered)
	}
	if prefixes[0] != revokedFallbackPrefix {
		t.Fatalf("prefix = %q, want revokedFallbackPrefix %q", prefixes[0], revokedFallbackPrefix)
	}

	// A second call (every subsequent tick, while still revoked) must not redeliver anything.
	handleLinkRevocation(fleet.LinkStatus{State: fleet.LinkRevoked}, lease, h, nil, fallback)
	if len(delivered) != 1 {
		t.Fatalf("delivered after second call = %+v, want still just 1 (no double-delivery)", delivered)
	}

	// Not revoked: a no-op, even with something pending.
	lease2 := newLeaseHolder(nowFn)
	lease2.Grant(1_000_000)
	h2 := newHandoff(nowFn, func() time.Duration { return time.Hour }, lease2)
	h2.Route(Alert{Key: "disk", Kind: "fire", Time: 1000})
	var calls int
	handleLinkRevocation(fleet.LinkStatus{State: fleet.LinkLinked}, lease2, h2, nil, func(Alert, *pushedSilences, string) { calls++ })
	if calls != 0 || !lease2.Valid() {
		t.Fatal("handleLinkRevocation must be a no-op when the link is not revoked")
	}
}

// TestRouteAfterRevokeAlwaysDeliversLocallyWithNoFallbackPrefix: once lease.Revoke has run,
// Route returns true unconditionally.
func TestRouteAfterRevokeAlwaysDeliversLocallyWithNoFallbackPrefix(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000)
	h := newHandoff(nowFn, func() time.Duration { return time.Hour }, lease)

	lease.Revoke()

	revokedNotice := Alert{Key: "fleet:link:revoked", Title: "⛔ This node was revoked by the fleet master.", Kind: "fire", Time: 1001}
	if !h.Route(revokedNotice) {
		t.Fatal("Route for the revoked node's own notice must return true once the lease is revoked")
	}
	if revokedNotice.Title != "⛔ This node was revoked by the fleet master." {
		t.Fatalf("title = %q, want unchanged -- Route never touches the title, only deliverFallback does", revokedNotice.Title)
	}

	later := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 2000}
	if !h.Route(later) {
		t.Fatal("Route for a later alert must also return true: revocation is terminal")
	}
	if later.Title != "CPU high" {
		t.Fatalf("title = %q, want unchanged", later.Title)
	}
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick = %v, want none pending -- both alerts were delivered directly, never routed", got)
	}
}

func TestHandoffRouteWhileLeaseValidHoldsBackLocalDelivery(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1090)
	h := newHandoff(nowFn, func() time.Duration { return 2 * time.Minute }, lease)

	a := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000}
	if h.Route(a) {
		t.Fatal("Route while lease valid must return false (do not deliver locally)")
	}
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick immediately after Route = %v, want none (neither fallback nor lease expiry has happened)", got)
	}
}

// child with no stream at all (an old master) or before the first lease frame ever arrives
// from a new one: leaseHolder.until stays 0, so Route must behave exactly like today.
func TestHandoffRouteWithoutAnyLeaseEverGrantedDeliversLocally(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn) // never Grant()ed
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	if !h.Route(a) {
		t.Fatal("no lease ever granted: Route must return true (local delivery, today's behaviour)")
	}
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick = %v, want none: Route never recorded anything pending", got)
	}
}

func TestHandoffReceiptWithinFallbackNeverDeliversLocally(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000) // stays valid for the whole test
	h := newHandoff(nowFn, func() time.Duration { return 2 * time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	h.Route(a)
	h.Receipt("cpu", 1000)

	now = time.Unix(1000+1000, 0) // long past fallback_after; receipt must have cancelled it
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a receipt = %v, want none", got)
	}
}

func TestHandoffTickFallsBackAfterFallbackAfterElapses(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000) // stays valid: fallback_after, not lease expiry, must trigger this
	h := newHandoff(nowFn, func() time.Duration { return 2 * time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	h.Route(a)

	now = time.Unix(1000+119, 0)
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick before fallback_after elapsed = %v, want none", got)
	}

	now = time.Unix(1000+120, 0)
	got := h.Tick()
	if len(got) != 1 || got[0].Key != "cpu" {
		t.Fatalf("Tick once fallback_after elapsed = %v, want the alert delivered once", got)
	}

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick again = %v, want none: it must not be reported (and so delivered) twice", got)
	}
}

// The lease expiring outright must fall an alert back to local delivery
// immediately, independent of (and well before) fallback_after.
func TestHandoffTickImmediateOnLeaseExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1010) // valid only 10s
	h := newHandoff(nowFn, func() time.Duration { return 2 * time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	h.Route(a)

	now = time.Unix(1011, 0) // lease just expired; fallback_after (2m) nowhere close
	got := h.Tick()
	if len(got) != 1 || got[0].Key != "cpu" {
		t.Fatalf("Tick once the lease expired = %v, want immediate fallback delivery", got)
	}
}

// A recover sharing a fire's key but with its own Time is tracked (and can fall back)
// independently: acking the fire must not silently ack the recover too.
func TestHandoffRecoverFollowsSamePathAsFireIndependently(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000)
	h := newHandoff(nowFn, func() time.Duration { return 60 * time.Second }, lease)

	fire := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	if h.Route(fire) {
		t.Fatal("fire: Route while lease valid must return false")
	}
	h.Receipt("cpu", 1000) // the master delivered the fire

	now = time.Unix(1050, 0) // the recover actually fires 50s later
	recover := Alert{Key: "cpu", Kind: "recover", Time: 1050}
	if h.Route(recover) {
		t.Fatal("recover: Route while lease valid must return false")
	}

	now = time.Unix(1000+61, 0) // past the fire's own fallback window, not the recover's
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick = %v, want none yet (fire was receipted; recover's window isn't up)", got)
	}

	now = time.Unix(1050+61, 0) // past the recover's own fallback window
	got := h.Tick()
	if len(got) != 1 || got[0].Kind != "recover" || got[0].Time != 1050 {
		t.Fatalf("Tick = %v, want the recover delivered once, on its own schedule", got)
	}
}

// --- enqueueAndLog x alertRoute -------------------------------------------

// Solo and master never call setAlertRoute, so alertRoute stays nil: this pins that
// enqueueAndLog's default (no hook installed) is exactly today's behaviour.
func TestEnqueueAndLogDeliversLocallyWhenNoRouteInstalled(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1}
	enqueueAndLog(alog, nil, q, a, false)

	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].RoutedToMaster {
		t.Fatalf("logged event = %+v, want RoutedToMaster=false", events)
	}
	if keys := q.snapshotKeysForTest(); len(keys) != 1 || keys[0] != "cpu" {
		t.Fatalf("queue = %v, want the alert enqueued locally", keys)
	}
}

func TestEnqueueAndLogRoutesToMasterWhenAlertRouteReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	restore := setAlertRoute(func(Alert) bool { return false })
	defer restore()

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 12345}
	enqueueAndLog(alog, nil, q, a, false)

	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || !events[0].RoutedToMaster || events[0].FiredAt != 12345 {
		t.Fatalf("logged event = %+v, want RoutedToMaster=true FiredAt=12345", events)
	}
	if keys := q.snapshotKeysForTest(); len(keys) != 0 {
		t.Fatalf("queue = %v, want nothing enqueued locally while routed to master", keys)
	}
}

// setAlertRoute's restore func must put back the PREVIOUS hook, not unconditionally clear
// it -- so nested installs (e.g. a test wrapping another) don't clobber each other.
func TestSetAlertRouteRestoresPrevious(t *testing.T) {
	restore1 := setAlertRoute(func(Alert) bool { return false })
	restore2 := setAlertRoute(func(Alert) bool { return true })
	restore2()
	if route := alertRoute.Load(); route == nil || (*route)(Alert{}) != false {
		t.Fatal("restore2 must put back the false-returning route installed before it")
	}
	restore1()
	if route := alertRoute.Load(); route != nil {
		t.Fatal("restore1 must put back nil (nothing was installed before it)")
	}
}

// --- deliverFallback -------------------------------------------------------

func TestDeliverFallbackPrefixesTitleAndRecordsSecondEvent(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1000}
	deliverFallback(nil, alog, nil, q, a, false, 1200, fallbackPrefix)

	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1", events)
	}
	e := events[0]
	wantTitle := "via local fallback: master unreachable — CPU high"
	if e.Title != wantTitle {
		t.Fatalf("title = %q, want %q", e.Title, wantTitle)
	}
	if !e.DeliveredLocally {
		t.Fatal("want DeliveredLocally = true")
	}
	if e.FiredAt != 1000 {
		t.Fatalf("FiredAt = %d, want 1000 (the original alert's fire time)", e.FiredAt)
	}
	if e.Time != 1200 {
		t.Fatalf("Time = %d, want 1200 (this fallback delivery's own time)", e.Time)
	}
	keys := q.snapshotKeysForTest()
	if len(keys) != 1 || keys[0] != "cpu" {
		t.Fatalf("queue = %v, want the alert delivered locally", keys)
	}
}

// --- onStreamFrame ----------------------------------------------------------

func TestOnStreamFrameLease(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)

	if lease.Valid() {
		t.Fatal("no lease frame processed yet, want invalid")
	}
	onStreamFrame(lease, h, noReceiptsPath(t), nil, nowFn, fleet.Frame{Type: "lease", Data: json.RawMessage(`{"until":1090}`)})
	if !lease.Valid() {
		t.Fatal("want valid after a lease frame")
	}
}

func TestOnStreamFrameReceipt(t *testing.T) {
	receiptsPath := filepath.Join(t.TempDir(), "handoff-receipts.jsonl")
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(2000)
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	h.Route(a)
	now = time.Unix(1005, 0)
	onStreamFrame(lease, h, receiptsPath, nil, nowFn, fleet.Frame{Type: "receipt", Data: json.RawMessage(`{"key":"cpu","fired_at":1000}`)})

	now = time.Unix(2000, 0) // long past fallback_after; a real receipt must have cancelled it
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a receipt frame = %v, want none", got)
	}

	// The receipt must also be durably recorded in the SIDECAR (never alertlog.jsonl), so a
	// restart doesn't resurrect this alert as pending (reconcilePendingFromLog).
	receipts, err := readHandoffReceipts(receiptsPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("sidecar receipts = %+v, want 1", receipts)
	}
	r := receipts[0]
	if r.Key != "cpu" || r.FiredAt != 1000 || r.Kind != "fire" || r.TS != 1005 {
		t.Fatalf("receipt = %+v, want key=cpu fired_at=1000 kind=fire ts=1005", r)
	}
}

// A receipt for something not (or no longer) pending must not be recorded in the sidecar at
// all: there is nothing meaningful to resolve.
func TestOnStreamFrameReceiptForNothingPendingRecordsNothing(t *testing.T) {
	receiptsPath := filepath.Join(t.TempDir(), "handoff-receipts.jsonl")
	lease := newLeaseHolder(nil)
	h := newHandoff(nil, func() time.Duration { return time.Minute }, lease)

	onStreamFrame(lease, h, receiptsPath, nil, time.Now, fleet.Frame{Type: "receipt", Data: json.RawMessage(`{"key":"cpu","fired_at":1000}`)})

	receipts, err := readHandoffReceipts(receiptsPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("sidecar receipts = %+v, want none", receipts)
	}
}

// Garbage or an unknown frame type must never panic or otherwise disrupt
// the stream's read loop (OnFrame's contract).
func TestOnStreamFrameIgnoresGarbageAndUnknownTypes(t *testing.T) {
	lease := newLeaseHolder(nil)
	h := newHandoff(nil, func() time.Duration { return time.Minute }, lease)
	onStreamFrame(lease, h, noReceiptsPath(t), nil, time.Now, fleet.Frame{Type: "lease", Data: json.RawMessage(`not json`)})
	onStreamFrame(lease, h, noReceiptsPath(t), nil, time.Now, fleet.Frame{Type: "receipt", Data: json.RawMessage(`not json`)})
	onStreamFrame(lease, h, noReceiptsPath(t), nil, time.Now, fleet.Frame{Type: "silences", Data: json.RawMessage(`{}`)})
	if lease.Valid() {
		t.Fatal("garbage lease data must not grant a lease")
	}
}

// deliverFallback must not mutate the caller's Alert (Title/Time are rewritten on a local
// copy): Go passes Alert by value.
func TestDeliverFallbackDoesNotMutateCallersAlert(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000}
	orig := a
	deliverFallback(nil, alog, nil, q, a, false, 1200, fallbackPrefix)
	if !reflect.DeepEqual(a, orig) {
		t.Fatalf("caller's Alert mutated: got %+v, want unchanged %+v", a, orig)
	}
}

// --- reconcilePendingFromLog (restart safety) -------------------------------

func routedFireEvent(key, title string, firedAt int64) AlertEvent {
	return AlertEvent{
		Time: firedAt, Key: key, Title: title, Severity: "warning", Kind: "fire",
		Source: "anomaly", RoutedToMaster: true, FiredAt: firedAt,
	}
}

func routedRecoverEvent(key, title string, firedAt int64) AlertEvent {
	return AlertEvent{
		Time: firedAt, Key: key, Title: title, Severity: "warning", Kind: "recover",
		Source: "anomaly", RoutedToMaster: true, FiredAt: firedAt,
	}
}

// noReceiptsPath is a receipts-sidecar path for tests that don't need any receipts
// recorded: readHandoffReceipts treats a missing file as "none".
func noReceiptsPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "handoff-receipts.jsonl")
}

func writeReceipt(t *testing.T, path, key string, firedAt int64, kind string, ts int64) {
	t.Helper()
	if err := appendHandoffReceipt(path, handoffReceipt{Key: key, FiredAt: firedAt, Kind: kind, TS: ts}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcilePendingFromLogFindsUnresolvedRoutedFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1010, 0))
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Time != 1000 || got[0].Title != "CPU high" || got[0].Severity != SevWarning || got[0].Kind != "fire" {
		t.Fatalf("reconcile = %+v, want the one unresolved routed fire", got)
	}
}

func TestReconcilePendingFromLogFindsUnresolvedRoutedRecover(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", 1000)); err != nil {
		t.Fatal(err)
	}

	got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1010, 0))
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Time != 1000 || got[0].Kind != "recover" {
		t.Fatalf("reconcile = %+v, want the one unresolved routed recover", got)
	}
}

// A fire that was NOT routed to the master (RoutedToMaster=false: solo, master, or a child
// with no lease at the time) was delivered locally already, synchronously.
func TestReconcilePendingFromLogIgnoresUnroutedFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	ev := routedFireEvent("cpu", "CPU high", 1000)
	ev.RoutedToMaster = false
	if err := alog.AppendAlertEvent(ev); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1010, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none (never routed)", got)
	}
}

func TestReconcilePendingFromLogSkipsReceiptResolvedFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	writeReceipt(t, receiptsPath, "cpu", 1000, "fire", 1005)

	if got := reconcilePendingFromLog(alog, receiptsPath, time.Minute, time.Unix(1010, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a sidecar receipt resolves it", got)
	}
}

func TestReconcilePendingFromLogSkipsReceiptResolvedRecover(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", 1000)); err != nil {
		t.Fatal(err)
	}
	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	writeReceipt(t, receiptsPath, "cpu", 1000, "recover", 1005)

	if got := reconcilePendingFromLog(alog, receiptsPath, time.Minute, time.Unix(1010, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a sidecar receipt resolves the routed recover too", got)
	}
}

func TestReconcilePendingFromLogSkipsDeliveredLocallyResolved(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 1065, Key: "cpu", Title: fallbackPrefix + "CPU high", Severity: "warning", Kind: "fire",
		Source: "anomaly", DeliveredLocally: true, FiredAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1070, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a delivered_locally record resolves it", got)
	}
}

// A delivered_locally record for a RECOVER (Kind stays "recover" through
// deliverFallback -- only Title/Time are rewritten) resolves a pending
// recover exactly like the fire case above.
func TestReconcilePendingFromLogSkipsDeliveredLocallyResolvedRecover(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 1065, Key: "cpu", Title: fallbackPrefix + "CPU back to normal", Severity: "warning", Kind: "recover",
		Source: "anomaly", DeliveredLocally: true, FiredAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1070, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a delivered_locally recover record resolves it", got)
	}
}

func TestReconcilePendingFromLogSkipsRecoveredAfterFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 1030, Key: "cpu", Title: "CPU back to normal", Severity: "warning", Kind: "recover", Source: "anomaly",
	}); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1040, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a later recover for the same key resolves the fire", got)
	}
}

// The unrouted recover above still gets its OWN entry properly resolved
// (RoutedToMaster=false means it was never pending in the first place).
func TestReconcilePendingFromLogUnroutedRecoverIsNotItselfPending(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 1030, Key: "cpu", Title: "CPU back to normal", Severity: "warning", Kind: "recover", Source: "anomaly",
	}); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1040, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none pending at all", got)
	}
}

// A recover logged BEFORE the fire it shares a key with (a stale/reordered
// line, or simply an earlier unrelated recover) must not resolve it.
func TestReconcilePendingFromLogRecoverBeforeFireDoesNotResolveIt(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: 900, Key: "cpu", Title: "CPU back to normal", Severity: "warning", Kind: "recover", Source: "anomaly",
	}); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1010, 0))
	if len(got) != 1 || got[0].Time != 1000 {
		t.Fatalf("reconcile = %+v, want the fire still pending (the recover predates it)", got)
	}
}

// A routed recover must NOT resolve another pending recover for the same key (recovers
// don't chain/resolve each other -- only a fire is resolved by a later recover).
func TestReconcilePendingFromLogRecoverDoesNotResolveAnotherPendingRecover(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal again", 1030)); err != nil {
		t.Fatal(err)
	}

	got := reconcilePendingFromLog(alog, noReceiptsPath(t), time.Minute, time.Unix(1040, 0))
	if len(got) != 2 {
		t.Fatalf("reconcile = %+v, want both routed recovers still pending", got)
	}
}

// The scan is bounded to the last fallbackAfter*10 window: a routed fire far outside it
// must not slow startup or resurrect a years-old, long-resolved alert.
func TestReconcilePendingFromLogBoundedByWindow(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := time.Minute // window = 10 * 60s = 600s
	receiptsPath := noReceiptsPath(t)
	now := time.Unix(1000+601, 0)
	if got := reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: the fire is outside the scan window", got)
	}
	now = time.Unix(1000+599, 0)
	if got := reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now); len(got) != 1 {
		t.Fatalf("reconcile = %+v, want the fire still found just inside the window", got)
	}
}

// A receipt whose own TS falls outside the scan window is treated as if it were never
// recorded: the fire it would have resolved -- itself still inside the window.
func TestReconcilePendingFromLogReceiptOutsideWindowIsIgnored(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	writeReceipt(t, receiptsPath, "cpu", 1000, "fire", 100) // an implausibly old TS, for the test

	fallbackAfter := time.Minute // window = 600s
	now := time.Unix(1550, 0)    // since = 950: the fire (1000) is in, the receipt (TS 100) is not
	got := reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now)
	if len(got) != 1 || got[0].Key != "cpu" {
		t.Fatalf("reconcile = %+v, want the fire still pending: its resolving receipt fell outside the scan window", got)
	}
}

func TestReconcilePendingFromLogNilAlogReturnsNil(t *testing.T) {
	if got := reconcilePendingFromLog(nil, noReceiptsPath(t), time.Minute, time.Now()); got != nil {
		t.Fatalf("reconcile with nil alog = %v, want nil", got)
	}
}

// --- full restart flow -------------

// Scenario 1: the child restarts mid-handoff with no receipt ever logged.
func TestRestartWithNoReceiptDeliversLocallyOnceAfterFallback(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	fireTime := int64(1000)
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", fireTime)); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := 60 * time.Second
	receiptsPath := noReceiptsPath(t)

	// "Restart": a fresh handoff/lease, reconciled from the log the way
	// startChild does, with a FRESH valid lease already re-granted (so it is
	// fallback timing -- not lease expiry -- that must trigger delivery).
	now := time.Unix(fireTime+30, 0) // fallback_after not yet elapsed
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(now.Unix() + 1000)
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick before fallback_after (measured from the ORIGINAL fire) elapsed = %v, want none yet", got)
	}

	now = time.Unix(fireTime+61, 0) // now past it
	got := h.Tick()
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Time != fireTime {
		t.Fatalf("Tick once fallback_after (from the original fire) elapsed = %+v, want the reconciled alert delivered once", got)
	}

	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	deliverFallback(nil, alog, nil, q, got[0], false, now.Unix(), fallbackPrefix)

	wantTitle := fallbackPrefix + "CPU high"
	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, e := range events {
		if e.DeliveredLocally {
			delivered++
			if e.Title != wantTitle || e.FiredAt != fireTime {
				t.Fatalf("delivered_locally event = %+v, want title %q fired_at %d", e, wantTitle, fireTime)
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered_locally events = %d, want exactly 1", delivered)
	}
	if keys := q.snapshotKeysForTest(); len(keys) != 1 || keys[0] != "cpu" {
		t.Fatalf("queue = %v, want the alert delivered locally exactly once", keys)
	}
	if got2 := h.Tick(); len(got2) != 0 {
		t.Fatalf("second Tick = %v, want none: already delivered, must not repeat", got2)
	}
}

// Scenario 2: same as above, but the receipt was logged (in the sidecar --
// never alertlog.jsonl) before the restart. No local delivery, ever.
func TestRestartWithReceiptLoggedBeforeRestartNeverDeliversLocally(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	fireTime := int64(1000)
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", fireTime)); err != nil {
		t.Fatal(err)
	}
	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	writeReceipt(t, receiptsPath, "cpu", fireTime, "fire", fireTime+2)

	fallbackAfter := 60 * time.Second
	now := time.Unix(fireTime+1000, 0) // long past fallback_after and lease expiry
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn) // no lease held after restart either
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a pre-restart receipt = %v, want none: never deliver locally", got)
	}
}

// Scenario 2b: the same, but for a routed RECOVER instead of a fire -- the "never zero
// times" rule covers recovers too.
func TestRestartWithRecoverReceiptLoggedBeforeRestartNeverDeliversLocally(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	firedAt := int64(1000)
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", firedAt)); err != nil {
		t.Fatal(err)
	}
	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	writeReceipt(t, receiptsPath, "cpu", firedAt, "recover", firedAt+2)

	fallbackAfter := 60 * time.Second
	now := time.Unix(firedAt+1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, receiptsPath, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a pre-restart recover receipt = %v, want none: never deliver locally", got)
	}
}

// Scenario 3: same as above, but a delivered_locally record is already
// present (the fallback delivery happened, then the process restarted
// again before Tick's normal single-delivery guard would matter). No
// second delivery.
func TestRestartWithDeliveredLocallyAlreadyPresentNoSecondDelivery(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	fireTime := int64(1000)
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", fireTime)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{
		Time: fireTime + 65, Key: "cpu", Title: fallbackPrefix + "CPU high", Severity: "warning", Kind: "fire",
		Source: "anomaly", DeliveredLocally: true, FiredAt: fireTime,
	}); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := 60 * time.Second
	now := time.Unix(fireTime+1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, noReceiptsPath(t), fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a pre-restart delivered_locally record = %v, want none: no second delivery", got)
	}
}

// Scenario 4: a routed RECOVER that restarts with no receipt ever logged is delivered
// locally exactly once, with the prefix -- exactly like scenario 1, but for a recover.
func TestRestartRecoverWithNoReceiptDeliversLocallyOnceAfterFallback(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	firedAt := int64(1000)
	if err := alog.AppendAlertEvent(routedRecoverEvent("cpu", "CPU back to normal", firedAt)); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := 60 * time.Second
	now := time.Unix(firedAt+30, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(now.Unix() + 1000) // fresh valid lease post-restart: fallback timing must still trigger this
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, noReceiptsPath(t), fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick before fallback_after elapsed = %v, want none yet", got)
	}

	now = time.Unix(firedAt+61, 0)
	got := h.Tick()
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Kind != "recover" || got[0].Time != firedAt {
		t.Fatalf("Tick once fallback_after elapsed = %+v, want the reconciled recover delivered once", got)
	}

	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	deliverFallback(nil, alog, nil, q, got[0], false, now.Unix(), fallbackPrefix)

	wantTitle := fallbackPrefix + "CPU back to normal"
	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, e := range events {
		if e.DeliveredLocally {
			delivered++
			if e.Title != wantTitle || e.FiredAt != firedAt || e.Kind != "recover" {
				t.Fatalf("delivered_locally event = %+v, want title %q fired_at %d kind recover", e, wantTitle, firedAt)
			}
		}
	}
	if delivered != 1 {
		t.Fatalf("delivered_locally events = %d, want exactly 1", delivered)
	}
	if keys := q.snapshotKeysForTest(); len(keys) != 1 || keys[0] != "cpu" {
		t.Fatalf("queue = %v, want the recover delivered locally exactly once", keys)
	}
	if got2 := h.Tick(); len(got2) != 0 {
		t.Fatalf("second Tick = %v, want none: already delivered, must not repeat", got2)
	}
}

// Receipts must never leak into alertlog.jsonl (which feeds alertHistoryRecords -- the web
// UI's /alerts history, `trinetra alerts list`) or the outbox.
func TestReceiptNeverLeaksIntoAlertlogOrOutbox(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	ob, err := fleet.OpenOutbox(filepath.Join(dir, "outbox"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer ob.Close()
	tee := newOutboxTee(ob, nil)
	alog.SetTee(tee.Alert)

	// The routed fire, exactly as enqueueAndLog (via alertRoute) would have
	// logged and teed it.
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	receiptsPath := filepath.Join(dir, "handoff-receipts.jsonl")
	now := time.Unix(1005, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(2000)
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)
	h.Route(Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000})

	onStreamFrame(lease, h, receiptsPath, nil, nowFn, fleet.Frame{
		Type: "receipt", Data: json.RawMessage(`{"key":"cpu","fired_at":1000}`),
	})

	// alertlog.jsonl (and so alertHistoryRecords) must show ONLY the fire --
	// no blank/receipt row.
	records, err := alertHistoryRecords(alog, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Key != "cpu" || records[0].Title != "CPU high" {
		t.Fatalf("alertHistoryRecords = %+v, want exactly the one fire, no receipt row", records)
	}

	// The outbox must also carry only the one alert record.
	recs, err := ob.Read(0, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Kind != fleet.KindAlert {
		t.Fatalf("outbox = %+v, want exactly the one alert record", recs)
	}

	// The receipt itself must still have been recorded -- just in the
	// sidecar, not alertlog/outbox.
	receipts, err := readHandoffReceipts(receiptsPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].Key != "cpu" || receipts[0].FiredAt != 1000 || receipts[0].Kind != "fire" {
		t.Fatalf("sidecar receipts = %+v, want one fire receipt for cpu@1000", receipts)
	}
}

// pruneHandoffReceipts drops receipts older than the cutoff and keeps the
// rest, mirroring AlertLog.PruneAlertLog.
func TestPruneHandoffReceiptsDropsOldKeepsRecent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff-receipts.jsonl")
	writeReceipt(t, path, "cpu", 100, "fire", 100)
	writeReceipt(t, path, "mem", 900, "recover", 900)

	if err := pruneHandoffReceipts(path, 500); err != nil {
		t.Fatal(err)
	}

	got, err := readHandoffReceipts(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "mem" {
		t.Fatalf("after prune = %+v, want only the recent (mem) receipt", got)
	}
}

// pruneHandoffReceipts on a sidecar that doesn't exist yet is a no-op, not
// an error (mirrors AlertLog.PruneAlertLog's own missing-file contract).
func TestPruneHandoffReceiptsMissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handoff-receipts.jsonl")
	if err := pruneHandoffReceipts(path, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("prune must not create the file when none exists (stat err=%v)", err)
	}
}

// Reconcile must not clobber a pending entry Route already created after construction but
// before the reconciliation scan ran (e.g. a fresh fire racing startup).
func TestHandoffReconcileDoesNotOverwriteAlreadyPendingEntry(t *testing.T) {
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(1_000_000)
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)

	fresh := Alert{Key: "cpu", Title: "fresh title", Kind: "fire", Time: 1000}
	h.Route(fresh) // Route's own routedAt = now (1000)

	stale := Alert{Key: "cpu", Title: "stale title from the log", Kind: "fire", Time: 1000}
	h.Reconcile([]Alert{stale})

	now = time.Unix(1061, 0) // past fallback_after
	got := h.Tick()
	if len(got) != 1 || got[0].Title != "fresh title" {
		t.Fatalf("Tick = %+v, want Route's own entry preserved, not overwritten by Reconcile", got)
	}
}

// --- pushed silences -----------------------------------

func TestPushedSilencesSuppressedMatchesRuleAndSeverity(t *testing.T) {
	p := newPushedSilences(filepath.Join(t.TempDir(), "silences.json"))
	if err := p.Set([]pushedSilence{
		{ID: "s1", Start: 1000, End: 2000, Reason: "silence s1 by cli"},
	}); err != nil {
		t.Fatal(err)
	}
	// An entry with no matchers (shouldn't normally happen; defensive)
	// matches nothing on the child side rather than panicking.
	if _, ok := p.Suppressed(1500, "cpu", "critical"); ok {
		t.Fatal("an entry with no matchers must not suppress anything")
	}

	if err := p.Set([]pushedSilence{
		{ID: "s2", Start: 1000, End: 2000, Reason: "silence s2 by cli", Matchers: []core.Matcher{{Rule: "cpu*", Severity: "critical"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if reason, ok := p.Suppressed(1500, "cpu_pct", "critical"); !ok || reason != "silence s2 by cli" {
		t.Fatalf("Suppressed = %q, %v, want silence s2 by cli, true", reason, ok)
	}
	if _, ok := p.Suppressed(1500, "mem_pct", "critical"); ok {
		t.Fatal("rule glob must not match an unrelated rule")
	}
	if _, ok := p.Suppressed(1500, "cpu_pct", "warning"); ok {
		t.Fatal("severity must be exact")
	}
	if _, ok := p.Suppressed(2500, "cpu_pct", "critical"); ok {
		t.Fatal("must not suppress once the window has ended")
	}
}

// TestPushedSilencesAppliesWhateverWasPushed: the child does not re-check Node
// against its own name (config.ServerName is not reliably the master's
// registry name, so a re-check risked wrongly delivering a silenced alert). It
// applies a pushed Matcher's Rule/Severity only, trusting the master already
// decided it applies to THIS node, even if the matcher carries a Node field
// the master resolved for some OTHER node.
func TestPushedSilencesAppliesWhateverWasPushed(t *testing.T) {
	p := newPushedSilences(filepath.Join(t.TempDir(), "silences.json"))
	// A matcher that would only ever have been resolved for a DIFFERENT node (Node:"db1") on
	// the real master.
	if err := p.Set([]pushedSilence{
		{ID: "s1", Start: 0, End: 5000, Reason: "silence s1 by cli", Matchers: []core.Matcher{{Node: "db1", Rule: "mem*"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if reason, ok := p.Suppressed(1000, "mem_pct", "warning"); !ok || reason != "silence s1 by cli" {
		t.Fatalf("Suppressed = %q, %v, want suppressed: the child trusts whatever was pushed, Node included", reason, ok)
	}
	if _, ok := p.Suppressed(1000, "cpu_pct", "warning"); ok {
		t.Fatal("rule glob must still not match an unrelated rule")
	}
}

func TestPushedSilencesNilReceiverIsNeverSuppressed(t *testing.T) {
	var p *pushedSilences
	if _, ok := p.Suppressed(1500, "cpu", "critical"); ok {
		t.Fatal("a nil pushedSilences (no push ever received) must never suppress")
	}
	if err := p.Set(nil); err != nil {
		t.Fatalf("Set on a nil receiver must be a no-op, not an error: %v", err)
	}
}

func TestPushedSilencesPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silences.json")
	p := newPushedSilences(path)
	if err := p.Set([]pushedSilence{{ID: "s1", Start: 1000, End: 2000, Reason: "maintenance patch", Matchers: []core.Matcher{{Rule: "*"}}}}); err != nil {
		t.Fatal(err)
	}
	reloaded := loadPushedSilences(path)
	if reason, ok := reloaded.Suppressed(1500, "anything", "warning"); !ok || reason != "maintenance patch" {
		t.Fatalf("reloaded Suppressed = %q, %v, want maintenance patch, true", reason, ok)
	}
}

func TestLoadPushedSilencesMissingFileStartsEmpty(t *testing.T) {
	p := loadPushedSilences(filepath.Join(t.TempDir(), "nope.json"))
	if _, ok := p.Suppressed(1000, "cpu", "warning"); ok {
		t.Fatal("a missing sidecar must start with no silences known")
	}
}

// --- deliverFallback honours a pushed silence ----------------------

func TestDeliverFallbackHonoursPushedSilenceSuppressesLocalDelivery(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	silences := newPushedSilences(filepath.Join(dir, "silences.json"))
	if err := silences.Set([]pushedSilence{
		{ID: "s1", Start: 0, End: 5000, Reason: "silence s1 by cli", Matchers: []core.Matcher{{Rule: "cpu*"}}},
	}); err != nil {
		t.Fatal(err)
	}

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1000}
	deliverFallback(silences, alog, nil, q, a, false, 1200, fallbackPrefix)

	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1", events)
	}
	e := events[0]
	if !e.DeliveredLocally {
		t.Fatal("a suppressed fallback must still be recorded DeliveredLocally=true, so the master never redelivers it")
	}
	if !strings.Contains(e.Title, "silenced (silence s1 by cli)") {
		t.Fatalf("title = %q, want it to note the suppression", e.Title)
	}
	if keys := q.snapshotKeysForTest(); len(keys) != 0 {
		t.Fatalf("queue = %v, want nothing actually delivered (silenced)", keys)
	}
}

func TestDeliverFallbackWithoutMatchingSilenceStillDelivers(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	silences := newPushedSilences(filepath.Join(dir, "silences.json"))
	if err := silences.Set([]pushedSilence{
		{ID: "s1", Start: 0, End: 5000, Reason: "silence s1 by cli", Matchers: []core.Matcher{{Rule: "mem*"}}},
	}); err != nil {
		t.Fatal(err)
	}

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1000}
	deliverFallback(silences, alog, nil, q, a, false, 1200, fallbackPrefix)

	if keys := q.snapshotKeysForTest(); len(keys) != 1 || keys[0] != "cpu" {
		t.Fatalf("queue = %v, want the alert delivered locally (no matching silence)", keys)
	}
}

// --- onStreamFrame applies a "silences" frame ----------------------

func TestOnStreamFrameSilencesUpdatesPushedSetAndPersists(t *testing.T) {
	dir := t.TempDir()
	silencesPath := filepath.Join(dir, "silences.json")
	silences := newPushedSilences(silencesPath)
	lease := newLeaseHolder(nil)
	h := newHandoff(nil, func() time.Duration { return time.Minute }, lease)

	frameData, err := json.Marshal(silencesFrameData{Silences: []pushedSilence{
		{ID: "s1", Start: 1000, End: 2000, Reason: "silence s1 by cli", Matchers: []core.Matcher{{Rule: "cpu*"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	onStreamFrame(lease, h, noReceiptsPath(t), silences, time.Now, fleet.Frame{Type: "silences", Data: frameData})

	if reason, ok := silences.Suppressed(1500, "cpu_pct", "warning"); !ok || reason != "silence s1 by cli" {
		t.Fatalf("in-memory Suppressed = %q, %v, want silence s1 by cli, true", reason, ok)
	}

	reloaded := loadPushedSilences(silencesPath)
	if _, ok := reloaded.Suppressed(1500, "cpu_pct", "warning"); !ok {
		t.Fatal("the silences frame must also be persisted to the sidecar (0600), for restart safety")
	}

	info, err := os.Stat(silencesPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("sidecar perm = %v, want 0600", info.Mode().Perm())
	}
}
