package serverwatch

import (
	"os"
	"path/filepath"
	"testing"
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
