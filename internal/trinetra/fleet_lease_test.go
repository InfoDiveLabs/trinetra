package trinetra

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

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

// A skewed or misbehaving master's far-future lease grant must be clamped
// to leaseMaxDuration from this side's own clock, not trusted verbatim --
// otherwise it could grant an effectively endless lease that silently
// swallows every future alert.
func TestLeaseHolderGrantCapsAtMax(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLeaseHolder(func() time.Time { return now })
	l.Grant(1000 + 10_000)
	now = time.Unix(1000+121, 0) // past leaseMaxDuration (120s) from the grant
	if l.Valid() {
		t.Fatal("lease must be capped at leaseMaxDuration from now, not the master's far-future claim")
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

// child with no stream at all (an old master) or before the first lease
// frame ever arrives from a new one: leaseHolder.until stays 0, so Route
// must behave exactly like today -- always deliver locally.
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

// A recover sharing a fire's key but with its own Time is tracked (and can
// fall back) independently: acking the fire must not silently ack the
// recover too.
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

// Solo and master never call setAlertRoute, so alertRoute stays nil: this
// pins that enqueueAndLog's default (no hook installed) is exactly today's
// behaviour -- local delivery, RoutedToMaster false -- which is what makes
// leaving every other enqueueAndLog call site untouched safe.
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

// setAlertRoute's restore func must put back the PREVIOUS hook, not
// unconditionally clear it -- so nested installs (e.g. a test wrapping
// another) don't clobber each other.
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
	deliverFallback(alog, nil, q, a, false, 1200)

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
	onStreamFrame(lease, h, nil, nowFn, fleet.Frame{Type: "lease", Data: json.RawMessage(`{"until":1090}`)})
	if !lease.Valid() {
		t.Fatal("want valid after a lease frame")
	}
}

func TestOnStreamFrameReceipt(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	now := time.Unix(1000, 0)
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(2000)
	h := newHandoff(nowFn, func() time.Duration { return time.Minute }, lease)

	a := Alert{Key: "cpu", Kind: "fire", Time: 1000}
	h.Route(a)
	now = time.Unix(1005, 0)
	onStreamFrame(lease, h, alog, nowFn, fleet.Frame{Type: "receipt", Data: json.RawMessage(`{"key":"cpu","fired_at":1000}`)})

	now = time.Unix(2000, 0) // long past fallback_after; a real receipt must have cancelled it
	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a receipt frame = %v, want none", got)
	}

	// The receipt must also be durably recorded, so a restart doesn't
	// resurrect this alert as pending (reconcilePendingFromLog).
	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 receipt marker", events)
	}
	e := events[0]
	if e.Kind != receiptMarkerKind || e.Key != "cpu" || e.FiredAt != 1000 || e.Time != 1005 {
		t.Fatalf("receipt marker = %+v, want kind=%s key=cpu fired_at=1000 time=1005", e, receiptMarkerKind)
	}
}

// Garbage or an unknown frame type must never panic or otherwise disrupt
// the stream's read loop (OnFrame's contract).
func TestOnStreamFrameIgnoresGarbageAndUnknownTypes(t *testing.T) {
	lease := newLeaseHolder(nil)
	h := newHandoff(nil, func() time.Duration { return time.Minute }, lease)
	onStreamFrame(lease, h, nil, time.Now, fleet.Frame{Type: "lease", Data: json.RawMessage(`not json`)})
	onStreamFrame(lease, h, nil, time.Now, fleet.Frame{Type: "receipt", Data: json.RawMessage(`not json`)})
	onStreamFrame(lease, h, nil, time.Now, fleet.Frame{Type: "silences", Data: json.RawMessage(`{}`)})
	if lease.Valid() {
		t.Fatal("garbage lease data must not grant a lease")
	}
}

// deliverFallback must not mutate the caller's Alert (Title/Time are
// rewritten on a local copy): Go passes Alert by value, but this pins that
// contract so a future refactor to a pointer receiver doesn't silently
// break the caller's own copy (e.g. handoff.Tick's returned slice).
func TestDeliverFallbackDoesNotMutateCallersAlert(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Kind: "fire", Time: 1000}
	orig := a
	deliverFallback(alog, nil, q, a, false, 1200)
	if a != orig {
		t.Fatalf("caller's Alert mutated: got %+v, want unchanged %+v", a, orig)
	}
}

// --- reconcilePendingFromLog (restart safety) -------------------------------
//
// handoff.pending lives only in memory: these tests pin the fix for the gap
// where a child restarting between Route (lease valid, so nothing delivered
// locally) and a receipt would otherwise lose the alert -- delivered by
// neither the master nor, ever, locally.

func routedFireEvent(key, title string, firedAt int64) AlertEvent {
	return AlertEvent{
		Time: firedAt, Key: key, Title: title, Severity: "warning", Kind: "fire",
		Source: "anomaly", RoutedToMaster: true, FiredAt: firedAt,
	}
}

func TestReconcilePendingFromLogFindsUnresolvedRoutedFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1010, 0))
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Time != 1000 || got[0].Title != "CPU high" || got[0].Severity != SevWarning {
		t.Fatalf("reconcile = %+v, want the one unresolved routed fire", got)
	}
}

// A fire that was NOT routed to the master (RoutedToMaster=false: solo,
// master, or a child with no lease at the time) was delivered locally
// already, synchronously, when it first fired -- it must never be treated
// as still pending.
func TestReconcilePendingFromLogIgnoresUnroutedFire(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	ev := routedFireEvent("cpu", "CPU high", 1000)
	ev.RoutedToMaster = false
	if err := alog.AppendAlertEvent(ev); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1010, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none (never routed)", got)
	}
}

func TestReconcilePendingFromLogSkipsReceiptResolved(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{Time: 1005, Key: "cpu", Kind: receiptMarkerKind, Source: "fleet", FiredAt: 1000}); err != nil {
		t.Fatal(err)
	}

	if got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1010, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a receipt marker resolves it", got)
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

	if got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1070, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a delivered_locally record resolves it", got)
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

	if got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1040, 0)); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: a later recover for the same key resolves the fire", got)
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

	got := reconcilePendingFromLog(alog, time.Minute, time.Unix(1010, 0))
	if len(got) != 1 || got[0].Time != 1000 {
		t.Fatalf("reconcile = %+v, want the fire still pending (the recover predates it)", got)
	}
}

// The scan is bounded to the last fallbackAfter*10 window: a routed fire far
// outside it must not slow startup or resurrect a years-old, long-resolved
// alert.
func TestReconcilePendingFromLogBoundedByWindow(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", 1000)); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := time.Minute // window = 10 * 60s = 600s
	now := time.Unix(1000+601, 0)
	if got := reconcilePendingFromLog(alog, fallbackAfter, now); len(got) != 0 {
		t.Fatalf("reconcile = %+v, want none: the fire is outside the scan window", got)
	}
	now = time.Unix(1000+599, 0)
	if got := reconcilePendingFromLog(alog, fallbackAfter, now); len(got) != 1 {
		t.Fatalf("reconcile = %+v, want the fire still found just inside the window", got)
	}
}

func TestReconcilePendingFromLogNilAlogReturnsNil(t *testing.T) {
	if got := reconcilePendingFromLog(nil, time.Minute, time.Now()); got != nil {
		t.Fatalf("reconcile with nil alog = %v, want nil", got)
	}
}

// --- full restart flow (the review's three required scenarios) -------------

// Scenario 1: the child restarts mid-handoff with no receipt ever logged.
// After restart, once fallback_after has passed (judged against the
// ORIGINAL fire time, not restart time), the alert is delivered locally
// exactly once, with the fallback prefix.
func TestRestartWithNoReceiptDeliversLocallyOnceAfterFallback(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	fireTime := int64(1000)
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", fireTime)); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := 60 * time.Second

	// "Restart": a fresh handoff/lease, reconciled from the log the way
	// startChild does, with a FRESH valid lease already re-granted (so it is
	// fallback timing -- not lease expiry -- that must trigger delivery).
	now := time.Unix(fireTime+30, 0) // fallback_after not yet elapsed
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn)
	lease.Grant(now.Unix() + 1000)
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick before fallback_after (measured from the ORIGINAL fire) elapsed = %v, want none yet", got)
	}

	now = time.Unix(fireTime+61, 0) // now past it
	got := h.Tick()
	if len(got) != 1 || got[0].Key != "cpu" || got[0].Time != fireTime {
		t.Fatalf("Tick once fallback_after (from the original fire) elapsed = %+v, want the reconciled alert delivered once", got)
	}

	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	deliverFallback(alog, nil, q, got[0], false, now.Unix())

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

// Scenario 2: same as above, but the receipt was logged before the restart.
// No local delivery, ever.
func TestRestartWithReceiptLoggedBeforeRestartNeverDeliversLocally(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	fireTime := int64(1000)
	if err := alog.AppendAlertEvent(routedFireEvent("cpu", "CPU high", fireTime)); err != nil {
		t.Fatal(err)
	}
	if err := alog.AppendAlertEvent(AlertEvent{Time: fireTime + 2, Key: "cpu", Kind: receiptMarkerKind, Source: "fleet", FiredAt: fireTime}); err != nil {
		t.Fatal(err)
	}

	fallbackAfter := 60 * time.Second
	now := time.Unix(fireTime+1000, 0) // long past fallback_after and lease expiry
	nowFn := func() time.Time { return now }
	lease := newLeaseHolder(nowFn) // no lease held after restart either
	h := newHandoff(nowFn, func() time.Duration { return fallbackAfter }, lease)
	h.Reconcile(reconcilePendingFromLog(alog, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a pre-restart receipt = %v, want none: never deliver locally", got)
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
	h.Reconcile(reconcilePendingFromLog(alog, fallbackAfter, now))

	if got := h.Tick(); len(got) != 0 {
		t.Fatalf("Tick after a pre-restart delivered_locally record = %v, want none: no second delivery", got)
	}
}

// Reconcile must not clobber a pending entry Route already created after
// construction but before the reconciliation scan ran (e.g. a fresh fire
// racing startup).
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
