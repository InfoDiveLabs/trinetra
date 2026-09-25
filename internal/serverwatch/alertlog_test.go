package serverwatch

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"serverwatch/internal/core"
)

func TestAlertLogAppendAndSinceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))

	ev1 := AlertEvent{
		Time:     100,
		Key:      "cpu",
		Title:    "CPU high",
		Severity: "warning",
		Kind:     "fire",
		Source:   "anomaly",
		Delivered: []Delivery{
			{Channel: "telegram", OK: true},
			{Channel: "email", OK: false, Err: "smtp timeout"},
		},
	}
	ev2 := AlertEvent{Time: 200, Key: "cpu", Title: "CPU back to normal", Severity: "warning", Kind: "recover", Source: "anomaly"}

	if err := l.AppendAlertEvent(ev1); err != nil {
		t.Fatal(err)
	}
	if err := l.AppendAlertEvent(ev2); err != nil {
		t.Fatal(err)
	}

	got, err := l.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(got), got)
	}
	if got[0].Key != "cpu" || got[0].Kind != "fire" {
		t.Fatalf("event[0] = %+v", got[0])
	}
	if len(got[0].Delivered) != 2 {
		t.Fatalf("event[0].Delivered = %+v, want 2 entries", got[0].Delivered)
	}
	if got[0].Delivered[0].Channel != "telegram" || !got[0].Delivered[0].OK {
		t.Fatalf("delivered[0] = %+v", got[0].Delivered[0])
	}
	if got[0].Delivered[1].Channel != "email" || got[0].Delivered[1].OK || got[0].Delivered[1].Err != "smtp timeout" {
		t.Fatalf("delivered[1] = %+v", got[0].Delivered[1])
	}
	if got[1].Kind != "recover" {
		t.Fatalf("event[1] = %+v", got[1])
	}

	// since filters out the earlier event
	got2, err := l.AlertEventsSince(150)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].Kind != "recover" {
		t.Fatalf("AlertEventsSince(150) = %+v", got2)
	}
}

func TestAlertLogSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alertlog.jsonl")
	content := "{\"time\":100,\"key\":\"cpu\",\"kind\":\"fire\"}\n" +
		"not json at all\n" +
		"{\"time\":200,\"key\":\"mem\",\"kind\":\"fire\"}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	l := NewAlertLog(path)
	got, err := l.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 valid events (corrupt line skipped), got %d: %+v", len(got), got)
	}
}

func TestAlertLogEventsSinceOnMissingFile(t *testing.T) {
	dir := t.TempDir()
	l := NewAlertLog(filepath.Join(dir, "missing.jsonl"))
	got, err := l.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("want nil for missing file, got %+v", got)
	}
}

func TestPruneAlertLogDropsOld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alertlog.jsonl")
	l := NewAlertLog(path)
	_ = l.AppendAlertEvent(AlertEvent{Time: 100, Key: "old", Kind: "fire"})
	_ = l.AppendAlertEvent(AlertEvent{Time: 1_000_000, Key: "recent", Kind: "fire"})

	if err := l.PruneAlertLog(500); err != nil {
		t.Fatal(err)
	}
	got, err := l.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "recent" {
		t.Fatalf("after prune = %+v, want only 'recent'", got)
	}
}

func TestDeliveriesFromMapsOKAndErr(t *testing.T) {
	results := []DeliveryResult{
		{Channel: "telegram", Err: nil},
		{Channel: "email", Err: errBoom{}},
	}
	got := deliveriesFrom(results)
	if len(got) != 2 {
		t.Fatalf("want 2 deliveries, got %d", len(got))
	}
	if got[0].Channel != "telegram" || !got[0].OK || got[0].Err != "" {
		t.Fatalf("deliveries[0] = %+v", got[0])
	}
	if got[1].Channel != "email" || got[1].OK || got[1].Err != "boom" {
		t.Fatalf("deliveries[1] = %+v", got[1])
	}
}

// errBoom is a trivial error used only to exercise deliveriesFrom's Err.Error() mapping.
type errBoom struct{}

func (errBoom) Error() string { return "boom" }

func TestPruneAlertLogMissingFileIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alertlog.jsonl")
	l := NewAlertLog(path)
	if err := l.PruneAlertLog(1000); err != nil {
		t.Fatalf("prune on missing file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("prune must not create the file when none exists (stat err=%v)", err)
	}
}

// TestEnqueueAndLogRecordsAlertEvent exercises the daemon glue: enqueueAndLog
// must append an AlertEvent carrying the alert fields. Delivery is off-thread
// now (the NotifierQueue worker owns it), so the log entry no longer carries a
// per-channel Delivered[] -- that field stays nil, and this test pins that
// contract change.
func TestEnqueueAndLogRecordsAlertEvent(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 12345}
	enqueueAndLog(alog, nil, q, a, false)

	events, err := alog.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("want 1 logged event, got %d", len(events))
	}
	e := events[0]
	if e.Key != "cpu" || e.Kind != "fire" || e.Severity != "warning" || e.Source != "anomaly" {
		t.Fatalf("logged event fields = %+v", e)
	}
	if len(e.Delivered) != 0 {
		t.Fatalf("delivery is off-thread now; Delivered must be nil, got %+v", e.Delivered)
	}
}

// TestEnqueueAndLogPublishesAlertFireEvent pins enqueueAndLog's publish
// side: an anomaly "fire" Alert (Source "anomaly") must also reach the event
// bus as a core.Event, with Kind mapped to "alert_fire" (not the bare
// Alert.Kind "fire") and Severity/Source/Title/Time carried straight across --
// the shape inprocAPI.Subscribe's control-socket consumers (and, eventually,
// the web UI's toast/refresh logic) key off.
func TestEnqueueAndLogPublishesAlertFireEvent(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	bus := newEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevCritical, Kind: "fire", Source: "anomaly", Time: 12345}
	enqueueAndLog(alog, bus, q, a, false)

	want := core.Event{Kind: "alert_fire", Severity: "critical", Source: "anomaly", Title: "CPU high", Time: 12345}
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("published event = %+v, want %+v", got, want)
		}
	default:
		t.Fatal("enqueueAndLog did not publish an event to the bus")
	}
}

// TestEnqueueAndLogPublishesAlertRecoverEvent is
// TestEnqueueAndLogPublishesAlertFireEvent's mirror image for a "recover"
// Alert.
func TestEnqueueAndLogPublishesAlertRecoverEvent(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	bus := newEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()

	a := Alert{Key: "cpu", Title: "CPU back to normal", Severity: SevWarning, Kind: "recover", Source: "anomaly", Time: 200}
	enqueueAndLog(alog, bus, q, a, false)

	want := core.Event{Kind: "alert_recover", Severity: "warning", Source: "anomaly", Title: "CPU back to normal", Time: 200}
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("published event = %+v, want %+v", got, want)
		}
	default:
		t.Fatal("enqueueAndLog did not publish an event to the bus")
	}
}

// TestEnqueueAndLogPublishesDigestKindVerbatim pins the "digests keep
// their kind" carve-out: a digest/boot-report Alert (Source != "anomaly")
// always carries Kind "fire" as a structural necessity (Alert.Kind has to
// be something), not because it's semantically a fire/recover anomaly
// transition -- so, unlike the anomaly case above, its Event.Kind is NOT
// remapped to "alert_fire"; it passes a.Kind straight through.
func TestEnqueueAndLogPublishesDigestKindVerbatim(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)
	bus := newEventBus()
	ch, cancel := bus.Subscribe()
	defer cancel()

	a := Alert{Title: "daily digest", Severity: SevInfo, Kind: "fire", Source: "digest", Time: 300}
	enqueueAndLog(alog, bus, q, a, false)

	want := core.Event{Kind: "fire", Severity: "info", Source: "digest", Title: "daily digest", Time: 300}
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("published event = %+v, want %+v", got, want)
		}
	default:
		t.Fatal("enqueueAndLog did not publish an event to the bus")
	}
}

// TestEnqueueAndLogNilBusIsSafe pins that a nil bus is a safe no-op, not a
// nil-pointer panic -- enqueueAndLog must keep working when there's no bus at
// all (mirroring eventBus.Publish's own nil-guard).
func TestEnqueueAndLogNilBusIsSafe(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	q := NewNotifierQueue(NewDispatcher(nil, time.Second), 8)

	a := Alert{Key: "cpu", Title: "CPU high", Severity: SevWarning, Kind: "fire", Source: "anomaly", Time: 1}
	enqueueAndLog(alog, nil, q, a, false)
}

// The sampler, the fleet master loop and the child link-alert goroutine all
// append concurrently. Writes must be serialized, and the tee (which ships
// alert history to the fleet master in outbox order) must see events in the
// same order they landed in the file.
func TestAlertLogConcurrentAppendsTeeInFileOrder(t *testing.T) {
	l := NewAlertLog(filepath.Join(t.TempDir(), "alertlog.jsonl"))
	var mu sync.Mutex
	var teed []string
	l.SetTee(func(ev AlertEvent) {
		runtime.Gosched()
		mu.Lock()
		teed = append(teed, ev.Key)
		mu.Unlock()
	})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = l.AppendAlertEvent(AlertEvent{Time: 1, Key: fmt.Sprintf("g%d-%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	evs, err := l.AlertEventsSince(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 400 || len(teed) != 400 {
		t.Fatalf("file has %d events, tee saw %d; want 400 each", len(evs), len(teed))
	}
	for i := range evs {
		if evs[i].Key != teed[i] {
			t.Fatalf("event %d: file %s, tee %s: tee order differs from file order", i, evs[i].Key, teed[i])
		}
	}
}
