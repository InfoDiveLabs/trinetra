package serverwatch

import (
	"path/filepath"
	"testing"
)

func TestAnomalyThresholdFireOnceThenRecover(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	chk := func(v float64) []Check {
		return []Check{{Key: "disk:/", Value: v, Threshold: 90, HasThreshold: true}}
	}
	// First breach -> one fire.
	ev := s.Evaluate(chk(95), b, 3, 100)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want 1 fire, got %+v", ev)
	}
	// Sustained breach -> no repeat.
	ev = s.Evaluate(chk(96), b, 3, 160)
	if len(ev) != 0 {
		t.Fatalf("sustained should be silent, got %+v", ev)
	}
	// Clears -> recover.
	ev = s.Evaluate(chk(80), b, 3, 220)
	if len(ev) != 1 || ev[0].Kind != "recover" {
		t.Fatalf("want recover, got %+v", ev)
	}
}

func TestAnomalyBaselineDeviation(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10, 5)
	}
	// No static threshold, but z spike must fire.
	ev := s.Evaluate([]Check{{Key: "cpu", Value: 70, Interval: 5}}, b, 3, 100)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want baseline fire, got %+v", ev)
	}
}

// TestAnomalyEvaluatePassesIntervalToObserve confirms Evaluate forwards each
// Check's Interval into Baseline.Observe, so the fast tier (short interval)
// and slow tier (long interval) each accumulate their own alpha rather than
// sharing one global constant.
func TestAnomalyEvaluatePassesIntervalToObserve(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	s.Evaluate([]Check{{Key: "cpu", Value: 10, Interval: 5}}, b, 3, 100)
	s.Evaluate([]Check{{Key: "disk:/", Value: 10, Interval: 60}}, b, 3, 100)
	if got, want := b.Stats["cpu"].Alpha, alphaFor(5); got != want {
		t.Fatalf("cpu alpha = %v, want %v (from Check.Interval=5)", got, want)
	}
	if got, want := b.Stats["disk:/"].Alpha, alphaFor(60); got != want {
		t.Fatalf("disk:/ alpha = %v, want %v (from Check.Interval=60)", got, want)
	}
	if b.Stats["cpu"].Alpha >= b.Stats["disk:/"].Alpha {
		t.Fatalf("cpu (5s) alpha %v should be < disk:/ (60s) alpha %v", b.Stats["cpu"].Alpha, b.Stats["disk:/"].Alpha)
	}
}

func TestAckSetsAckedAndAckedAt(t *testing.T) {
	s := NewAlertState()
	s.Active["disk:/"] = ActiveAlert{Since: 100, Reason: "disk:/ = 95.0 ≥ threshold 90.0"}

	if err := s.Ack("disk:/", 200); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	got := s.Active["disk:/"]
	if !got.Acked || got.AckedAt != 200 {
		t.Fatalf("after Ack: %+v", got)
	}
}

func TestAckOfNonActiveKeyErrors(t *testing.T) {
	s := NewAlertState()
	if err := s.Ack("disk:/", 200); err == nil {
		t.Fatal("expected error acking a key with no active alert")
	}
}

func TestUnackClearsAckState(t *testing.T) {
	s := NewAlertState()
	s.Active["disk:/"] = ActiveAlert{Since: 100, Reason: "r", Acked: true, AckedAt: 200}

	if err := s.Unack("disk:/"); err != nil {
		t.Fatalf("Unack: %v", err)
	}
	got := s.Active["disk:/"]
	if got.Acked || got.AckedAt != 0 {
		t.Fatalf("after Unack: %+v", got)
	}
}

func TestUnackOfNonActiveKeyErrors(t *testing.T) {
	s := NewAlertState()
	if err := s.Unack("disk:/"); err == nil {
		t.Fatal("expected error unacking a key with no active alert")
	}
}

// TestAckPersistsViaSaveLoadBackCompat confirms the new Acked/AckedAt fields
// round-trip through Save/LoadAlertState, and that an old alerts.json written
// before these fields existed still loads fine (back-compat).
func TestAckPersistsViaSaveLoadBackCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	fs := osFS{}

	s := NewAlertState()
	s.Active["cpu"] = ActiveAlert{Since: 100, Reason: "cpu hot"}
	if err := s.Ack("cpu", 150); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadAlertState(path, fs)
	got := reloaded.Active["cpu"]
	if !got.Acked || got.AckedAt != 150 {
		t.Fatalf("reloaded active alert = %+v, want Acked=true AckedAt=150", got)
	}

	// Old-format alerts.json, written before Acked/AckedAt existed.
	oldJSON := `{"active":{"mem":{"since":50,"reason":"mem high"}}}`
	oldPath := filepath.Join(dir, "old_alerts.json")
	if err := writeFileAtomic(oldPath, []byte(oldJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	old := LoadAlertState(oldPath, fs)
	memAlert, ok := old.Active["mem"]
	if !ok || memAlert.Acked || memAlert.Since != 50 {
		t.Fatalf("old-format load = %+v ok=%v, want present, not acked", memAlert, ok)
	}
}
