package trinetra

import (
	"math"
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
	ev := s.Evaluate(chk(95), b, 3, 0, true, 100)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want 1 fire, got %+v", ev)
	}
	// Sustained breach -> no repeat.
	ev = s.Evaluate(chk(96), b, 3, 0, true, 160)
	if len(ev) != 0 {
		t.Fatalf("sustained should be silent, got %+v", ev)
	}
	// Clears -> recover.
	ev = s.Evaluate(chk(80), b, 3, 0, true, 220)
	if len(ev) != 1 || ev[0].Kind != "recover" {
		t.Fatalf("want recover, got %+v", ev)
	}
}

// TestBreachUsesFireMsgWhenSet confirms a threshold breach returns the Check's FireMsg
// verbatim.
func TestBreachUsesFireMsgWhenSet(t *testing.T) {
	withMsg := Check{Key: "docker:web", Value: 1, Threshold: 1, HasThreshold: true, FireMsg: "container web is down (exited)"}
	if breach, reason := withMsg.breach(NewBaseline(), 3, 0, true); !breach || reason != "container web is down (exited)" {
		t.Fatalf("breach with FireMsg = (%v, %q), want (true, %q)", breach, reason, "container web is down (exited)")
	}

	noMsg := Check{Key: "disk:/", Value: 95, Threshold: 90, HasThreshold: true}
	if breach, reason := noMsg.breach(NewBaseline(), 3, 0, true); !breach || reason != "disk:/ = 95.0 ≥ threshold 90.0" {
		t.Fatalf("breach without FireMsg = (%v, %q), want numeric fallback", breach, reason)
	}
}

// TestEvaluateRecoverUsesRecoverMsgWhenSet confirms a recover transition's Event.Text is
// the Check's RecoverMsg when set, otherwise the default "<key> back to normal".
func TestEvaluateRecoverUsesRecoverMsgWhenSet(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	fire := Check{Key: "docker:web", Value: 1, Threshold: 1, HasThreshold: true, RecoverMsg: "container web recovered"}
	s.Evaluate([]Check{fire}, b, 3, 0, true, 100)

	recover := Check{Key: "docker:web", Value: 0, Threshold: 1, HasThreshold: true, RecoverMsg: "container web recovered"}
	ev := s.Evaluate([]Check{recover}, b, 3, 0, true, 160)
	if len(ev) != 1 || ev[0].Kind != "recover" || ev[0].Text != "container web recovered" {
		t.Fatalf("recover with RecoverMsg = %+v, want text %q", ev, "container web recovered")
	}

	// No RecoverMsg -> default wording.
	s2 := NewAlertState()
	b2 := NewBaseline()
	s2.Evaluate([]Check{{Key: "cpu", Value: 95, Threshold: 90, HasThreshold: true}}, b2, 3, 0, true, 100)
	ev2 := s2.Evaluate([]Check{{Key: "cpu", Value: 10, Threshold: 90, HasThreshold: true}}, b2, 3, 0, true, 160)
	if len(ev2) != 1 || ev2[0].Text != "cpu back to normal" {
		t.Fatalf("recover without RecoverMsg = %+v, want default wording", ev2)
	}
}

func TestAnomalyBaselineDeviation(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10, 5)
	}
	// No static threshold, but z spike must fire (baseline alerts enabled).
	ev := s.Evaluate([]Check{{Key: "cpu", Value: 70, Interval: 5}}, b, 3, 0, true, 100)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want baseline fire, got %+v", ev)
	}
}

// TestBaselineAlertsOffSuppressesDeviationButNotThreshold replays the field complaint (Part
// 1 of the baseline/strip/status fix): cpu/mem/temp have a low.
func TestBaselineAlertsOffSuppressesDeviationButNotThreshold(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10, 5)
	}
	// Sanity: this same spike would fire with baseline alerts enabled.
	z, ready := b.Z("cpu", 70)
	if !ready || math.Abs(z) < 3 {
		t.Fatalf("setup invariant broken: z=%v ready=%v, want a large ready z", z, ready)
	}

	// baseline_alerts=false: no threshold configured -> no event at all,
	// despite the huge sigma deviation.
	ev := s.Evaluate([]Check{{Key: "cpu", Value: 70, Interval: 5}}, b, 3, 0, false, 100)
	if len(ev) != 0 {
		t.Fatalf("baseline_alerts=false should suppress the deviation fire entirely, got %+v", ev)
	}

	// Threshold breach on the same key, same toggle off -> still fires.
	ev = s.Evaluate([]Check{{Key: "cpu", Value: 96, Threshold: 90, HasThreshold: true, Interval: 5}}, b, 3, 0, false, 160)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("threshold breach must fire regardless of baseline_alerts, got %+v", ev)
	}
}

// TestBaselineAlertsOnFiresSubjectToSigmaAndMinPct confirms baseline_alerts= true
// reproduces the pre-existing sigma+minPct-gated behavior exactly.
func TestBaselineAlertsOnFiresSubjectToSigmaAndMinPct(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	for i := 0; i < 400; i++ {
		v := 43.0
		if i%2 == 0 {
			v = 43.2
		} else {
			v = 42.8
		}
		b.Observe("temp", v, 5)
	}

	// Big z, but relative deviation ~4.6% < minPct 15% -> no breach.
	ev := s.Evaluate([]Check{{Key: "temp", Value: 45, Interval: 5}}, b, 3, 0.15, true, 100)
	if len(ev) != 0 {
		t.Fatalf("want no breach (below minPct gate) even with baseline_alerts=true, got %+v", ev)
	}
	// Far outlier clears both gates -> fires.
	ev = s.Evaluate([]Check{{Key: "temp", Value: 60, Interval: 5}}, b, 3, 0.15, true, 160)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want 1 fire (far outlier clears both gates), got %+v", ev)
	}
}

// TestAnomalyEvaluatePassesIntervalToObserve confirms Evaluate forwards each Check's
// Interval into Baseline.Observe, so the fast tier (short interval) and slow tier.
func TestAnomalyEvaluatePassesIntervalToObserve(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	s.Evaluate([]Check{{Key: "cpu", Value: 10, Interval: 5}}, b, 3, 0, true, 100)
	s.Evaluate([]Check{{Key: "disk:/", Value: 10, Interval: 60}}, b, 3, 0, true, 100)
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

// TestBaselineMinPctGateSuppressesNoisyStableMetric replays the field complaint.
func TestBaselineMinPctGateSuppressesNoisyStableMetric(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	// Tight baseline around 43; the tiny wobble keeps variance small so a 45
	// reads as a large z-score.
	for i := 0; i < 400; i++ {
		v := 43.0
		if i%2 == 0 {
			v = 43.2
		} else {
			v = 42.8
		}
		b.Observe("temp", v, 5)
	}
	z, ready := b.Z("temp", 45)
	if !ready {
		t.Fatal("baseline should be ready")
	}
	if math.Abs(z) < 3 {
		t.Fatalf("setup invariant broken: z = %v, want a large z for this test to be meaningful", z)
	}
	relDev := math.Abs(45-43) / 43.0
	if relDev >= 0.15 {
		t.Fatalf("setup invariant broken: relative deviation %v should be < 0.15", relDev)
	}

	// (a) value 45 (mean ~43): big z, but relative deviation ~4.6%% < 15% -> no breach.
	ev := s.Evaluate([]Check{{Key: "temp", Value: 45, Interval: 5}}, b, 3, 0.15, true, 100)
	if len(ev) != 0 {
		t.Fatalf("want no breach (relative deviation below minPct gate), got %+v", ev)
	}

	// (b) value 60 (mean ~43): both |z|>=sigma AND relative deviation (~39%%) >= 15% -> breach.
	ev = s.Evaluate([]Check{{Key: "temp", Value: 60, Interval: 5}}, b, 3, 0.15, true, 160)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("want 1 fire (far outlier clears both gates), got %+v", ev)
	}
}

// TestBaselineMinPctZeroPreservesPureSigmaBehavior is a regression guard: minPct=0 must
// reproduce the exact pre-existing pure-sigma behavior.
func TestBaselineMinPctZeroPreservesPureSigmaBehavior(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("cpu", 10, 5)
	}
	ev := s.Evaluate([]Check{{Key: "cpu", Value: 70, Interval: 5}}, b, 3, 0, true, 100)
	if len(ev) != 1 || ev[0].Kind != "fire" {
		t.Fatalf("minPct=0 should preserve pure-sigma fire, got %+v", ev)
	}
}

// TestBreachMinPctGateNearZeroMeanDoesNotPanic confirms the relative gate's division uses
// max(|mean|, meanFloor) so a metric whose baseline mean sits near zero.
func TestBreachMinPctGateNearZeroMeanDoesNotPanic(t *testing.T) {
	b := NewBaseline()
	for i := 0; i < 200; i++ {
		b.Observe("counter", 0, 5)
	}
	chk := Check{Key: "counter", Value: 0.05, Interval: 5}
	breach, _ := chk.breach(b, 3, 0.15, true)
	if breach {
		t.Fatalf("tiny absolute deviation from a near-zero mean should not breach with meanFloor guarding the relative gate")
	}
}

// TestEvaluateFireRecordsCriticalOnActiveAlert pins that Evaluate carries each firing
// Check's Critical flag onto its ActiveAlert.
func TestEvaluateFireRecordsCriticalOnActiveAlert(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	critChk := Check{Key: "disk:/", Value: 95, Threshold: 90, HasThreshold: true, Critical: true}
	warnChk := Check{Key: "cpu", Value: 96, Threshold: 90, HasThreshold: true, Critical: false}

	s.Evaluate([]Check{critChk, warnChk}, b, 3, 0, false, 100)

	if got := s.Active["disk:/"]; !got.Critical {
		t.Fatalf("disk:/ ActiveAlert.Critical = false, want true (fired from a Critical Check)")
	}
	if got := s.Active["cpu"]; got.Critical {
		t.Fatalf("cpu ActiveAlert.Critical = true, want false (fired from a non-critical Check)")
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

// TestAckPersistsViaSaveLoadBackCompat confirms the new Acked/AckedAt fields round-trip
// through Save/LoadAlertState.
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

// TestMergeAckFromDiskPreservesCLIAck replays the durability bug: the daemon holds
// AlertState in memory and re-saves on a fire/recover.
func TestMergeAckFromDiskPreservesCLIAck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	fs := osFS{}

	// Daemon's in-memory state with an active, un-acked alert, saved to disk.
	daemon := NewAlertState()
	daemon.Active["disk:/"] = ActiveAlert{Since: 100, Reason: "disk full"}
	if err := daemon.Save(path); err != nil {
		t.Fatal(err)
	}

	// CLI acks it: loads from disk, acks, saves back.
	cli := LoadAlertState(path, fs)
	if err := cli.Ack("disk:/", 150); err != nil {
		t.Fatal(err)
	}
	if err := cli.Save(path); err != nil {
		t.Fatal(err)
	}

	// Daemon now hits a fire/recover elsewhere and re-saves its (stale,
	// un-acked) in-memory copy. Without the merge this reverts the ack.
	daemon.MergeAckFromDisk(path, fs)
	if err := daemon.Save(path); err != nil {
		t.Fatal(err)
	}

	reloaded := LoadAlertState(path, fs)
	got := reloaded.Active["disk:/"]
	if !got.Acked || got.AckedAt != 150 {
		t.Fatalf("ack was reverted by daemon save: %+v", got)
	}
}

// TestMergeAckFromDiskIgnoresRemovedKeys confirms merge only touches keys still active in
// memory: a stale on-disk ack for a recovered/removed alert must not resurrect it.
func TestMergeAckFromDiskIgnoresRemovedKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alerts.json")
	fs := osFS{}

	onDisk := NewAlertState()
	onDisk.Active["cpu"] = ActiveAlert{Since: 1, Reason: "old", Acked: true, AckedAt: 5}
	if err := onDisk.Save(path); err != nil {
		t.Fatal(err)
	}

	// In memory, "cpu" has since recovered (absent) and "mem" is newly active.
	mem := NewAlertState()
	mem.Active["mem"] = ActiveAlert{Since: 200, Reason: "mem high"}
	mem.MergeAckFromDisk(path, fs)

	if _, ok := mem.Active["cpu"]; ok {
		t.Fatal("merge must not resurrect a removed key")
	}
	if mem.Active["mem"].Acked {
		t.Fatalf("mem should be untouched (absent from disk): %+v", mem.Active["mem"])
	}
}
