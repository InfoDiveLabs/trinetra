// fleet_lease_ack_test.go covers applyAckFrame (fleet_lease.go): the child's handling of
// the master's "ack"/"unack" stream frames (AckIncident's PushAck).
package trinetra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

func TestApplyAckFrameAcksActiveAlert(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)

	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000}
	if err := state.Save(filepath.Join(dir, "alerts.json")); err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(ackFrameData{Key: "cpu"})
	applyAckFrame(self, fleet.Frame{Type: "ack", Data: data})

	got := LoadAlertState(filepath.Join(dir, "alerts.json"), osFS{})
	a, ok := got.Active["cpu"]
	if !ok || !a.Acked {
		t.Fatalf("active alert after ack = %+v ok=%v", a, ok)
	}
}

func TestApplyAckFrameUnacksActiveAlert(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)

	state := NewAlertState()
	state.Active["cpu"] = ActiveAlert{Since: 1000, Acked: true, AckedAt: 1010}
	if err := state.Save(filepath.Join(dir, "alerts.json")); err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(ackFrameData{Key: "cpu"})
	applyAckFrame(self, fleet.Frame{Type: "unack", Data: data})

	got := LoadAlertState(filepath.Join(dir, "alerts.json"), osFS{})
	a, ok := got.Active["cpu"]
	if !ok || a.Acked {
		t.Fatalf("active alert after unack = %+v ok=%v", a, ok)
	}
}

func TestApplyAckFrameIgnoresOtherFrameTypesAndBadPayloads(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)

	// Neither call should panic or create alerts.json.
	applyAckFrame(self, fleet.Frame{Type: "lease", Data: json.RawMessage(`{"until":1}`)})
	applyAckFrame(self, fleet.Frame{Type: "ack", Data: json.RawMessage(`not json`)})
	applyAckFrame(nil, fleet.Frame{Type: "ack", Data: json.RawMessage(`{"key":"cpu"}`)})

	if _, err := os.Stat(filepath.Join(dir, "alerts.json")); err == nil {
		t.Fatal("alerts.json should not have been created")
	}
}
