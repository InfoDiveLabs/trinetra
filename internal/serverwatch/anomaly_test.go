package serverwatch

import "testing"

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
