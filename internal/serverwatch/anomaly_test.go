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

// TestBreachUsesFireMsgWhenSet confirms a threshold breach returns the
// Check's FireMsg verbatim (for binary health checks like docker/service/
// smart, which want human wording instead of the numeric comparison), and
// falls back to the numeric string when FireMsg is empty (regression guard
// for the existing numeric checks).
func TestBreachUsesFireMsgWhenSet(t *testing.T) {
	withMsg := Check{Key: "docker:web", Value: 1, Threshold: 1, HasThreshold: true, FireMsg: "container web is down (exited)"}
	if breach, reason := withMsg.breach(NewBaseline(), 3); !breach || reason != "container web is down (exited)" {
		t.Fatalf("breach with FireMsg = (%v, %q), want (true, %q)", breach, reason, "container web is down (exited)")
	}

	noMsg := Check{Key: "disk:/", Value: 95, Threshold: 90, HasThreshold: true}
	if breach, reason := noMsg.breach(NewBaseline(), 3); !breach || reason != "disk:/ = 95.0 ≥ threshold 90.0" {
		t.Fatalf("breach without FireMsg = (%v, %q), want numeric fallback", breach, reason)
	}
}

// TestEvaluateRecoverUsesRecoverMsgWhenSet confirms a recover transition's
// Event.Text is the Check's RecoverMsg when set, otherwise the default
// "<key> back to normal".
func TestEvaluateRecoverUsesRecoverMsgWhenSet(t *testing.T) {
	s := NewAlertState()
	b := NewBaseline()
	fire := Check{Key: "docker:web", Value: 1, Threshold: 1, HasThreshold: true, RecoverMsg: "container web recovered"}
	s.Evaluate([]Check{fire}, b, 3, 100)

	recover := Check{Key: "docker:web", Value: 0, Threshold: 1, HasThreshold: true, RecoverMsg: "container web recovered"}
	ev := s.Evaluate([]Check{recover}, b, 3, 160)
	if len(ev) != 1 || ev[0].Kind != "recover" || ev[0].Text != "container web recovered" {
		t.Fatalf("recover with RecoverMsg = %+v, want text %q", ev, "container web recovered")
	}

	// No RecoverMsg -> default wording.
	s2 := NewAlertState()
	b2 := NewBaseline()
	s2.Evaluate([]Check{{Key: "cpu", Value: 95, Threshold: 90, HasThreshold: true}}, b2, 3, 100)
	ev2 := s2.Evaluate([]Check{{Key: "cpu", Value: 10, Threshold: 90, HasThreshold: true}}, b2, 3, 160)
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

// TestMergeAckFromDiskPreservesCLIAck replays the durability bug: the daemon
// holds AlertState in memory and re-saves on a fire/recover; a CLI `alerts
// ack` written to disk in between must survive that save rather than being
// clobbered. MergeAckFromDisk (called just before Save) is what preserves it.
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

// TestMergeAckFromDiskIgnoresRemovedKeys confirms merge only touches keys
// still active in memory: a stale on-disk ack for a recovered/removed alert
// must not resurrect it.
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
