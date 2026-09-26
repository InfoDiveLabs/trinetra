package trinetra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// newTestMasterState builds a masterState with a real registry, engine,
// incident store, audit log and Hub (no live connections), for testing
// fleetAPIImpl's incident/audit surface without a full startFleet.
func newTestMasterState(t *testing.T) *masterState {
	t.Helper()
	dir := t.TempDir()
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	toks, err := fleet.OpenTokens(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(time.Minute))
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	audit := newAuditLog(filepath.Join(dir, "audit.jsonl"))
	hub := fleet.NewHub(nil)
	engine := newFleetAlertEngine(time.Now, func(Alert) bool { return true }, hub.Push, hub.Connected, incidents)
	loop := newMasterLoop(reg, tracker, sink, engine, fleetDeps{alert: func(Alert) bool { return true }}, time.Now())
	return &masterState{reg: reg, tokens: toks, tracker: tracker, sink: sink, loop: loop, hub: hub, engine: engine, audit: audit, getCfg: config.Default}
}

func fleetAPIFor(m *masterState) fleetAPIImpl {
	return fleetAPIImpl{&fleetProvider{role: config.RoleMaster, master: m}}
}

func TestFleetAPIAckIncidentAndAudit(t *testing.T) {
	m := newTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)

	m.engine.Submit(alertSource{NodeID: nodeID, NodeName: "web1"}, Alert{Key: "cpu", Kind: "fire", Severity: SevWarning, Time: 1000})

	incs, err := api.Incidents(core.IncidentFilter{})
	if err != nil || len(incs) != 1 {
		t.Fatalf("incidents = %+v err %v", incs, err)
	}
	id := incs[0].ID

	if err := api.AckIncident(id, "cli"); err != nil {
		t.Fatal(err)
	}
	inc, err := api.Incident(id)
	if err != nil || inc.State != "acked" || inc.AckedBy != "cli" {
		t.Fatalf("incident after ack = %+v err %v", inc, err)
	}

	entries, err := api.Audit(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "ack_incident" && e.Actor == "cli" && e.Target == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit missing ack_incident entry: %+v", entries)
	}

	if err := api.AckIncident("no-such-id", "cli"); err == nil {
		t.Fatal("AckIncident on an unknown id should fail")
	}
}

func TestFleetAPIAckIncidentSkipsResolvedAlertsAndSelfNode(t *testing.T) {
	m := newTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)

	// One still-open child alert, plus (in the same incident's group key
	// space, via a separate incident) a master-own alert with no node.
	m.engine.Submit(alertSource{NodeID: nodeID, NodeName: "web1"}, Alert{Key: "cpu", Kind: "fire", Severity: SevWarning, Time: 1000})
	m.engine.Submit(alertSource{}, Alert{Key: "fleet:node:x:down", Kind: "fire", Severity: SevCritical, Time: 1000})

	incs, _ := api.Incidents(core.IncidentFilter{})
	for _, inc := range incs {
		// AckIncident must not panic pushing to a self/master alert (no
		// node) or to an already-resolved one; both should simply be
		// skipped. Reaching this call without an error/panic is the
		// assertion.
		if err := api.AckIncident(inc.ID, "cli"); err != nil {
			t.Fatalf("AckIncident(%s) = %v", inc.ID, err)
		}
	}
}

func TestFleetAPIExplainByIncidentIDAndAlertKey(t *testing.T) {
	m := newTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)
	m.engine.Submit(alertSource{NodeID: nodeID, NodeName: "web1"}, Alert{Key: "cpu", Kind: "fire", Severity: SevWarning, Time: 1000})

	incs, _ := api.Incidents(core.IncidentFilter{})
	id := incs[0].ID

	byID, err := api.Explain(id)
	if err != nil || len(byID) == 0 {
		t.Fatalf("Explain(id) = %+v err %v", byID, err)
	}
	byKey, err := api.Explain("cpu")
	if err != nil || len(byKey) == 0 {
		t.Fatalf("Explain(key) = %+v err %v", byKey, err)
	}
	if _, err := api.Explain("no-such-key"); err == nil {
		t.Fatal("Explain on an unknown key/id should fail")
	}
}

func TestFleetAPIMutationsAudited(t *testing.T) {
	m := newTestMasterState(t)
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	m.joinURL = "https://master.example:9443" // required for CreateToken
	api := fleetAPIFor(m)

	if err := api.RenameNode(nodeID, "web1-renamed"); err != nil {
		t.Fatal(err)
	}
	if err := api.SetNodeTags(nodeID, []string{"prod"}); err != nil {
		t.Fatal(err)
	}
	if err := api.RevokeNode(nodeID); err != nil {
		t.Fatal(err)
	}
	tok, err := api.CreateToken(core.TokenSpec{Creator: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.DeleteToken(tok.Token.ID); err != nil {
		t.Fatal(err)
	}
	secondNode, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: secondNode, Name: "gone", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := api.RemoveNode(secondNode); err != nil {
		t.Fatal(err)
	}

	entries, err := api.Audit(0)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := map[string]string{
		"rename_node": "unknown", "set_node_tags": "unknown", "revoke_node": "unknown",
		"create_token": "cli", "delete_token": "unknown", "remove_node": "unknown",
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if wantActor, ok := wantActions[e.Action]; ok {
			if e.Actor != wantActor {
				t.Errorf("action %s actor = %q, want %q", e.Action, e.Actor, wantActor)
			}
			seen[e.Action] = true
		}
	}
	for action := range wantActions {
		if !seen[action] {
			t.Errorf("audit missing action %q; entries = %+v", action, entries)
		}
	}
}

// TestFleetAPIMutationsNilSafeWithoutOptionalFields guards against a panic
// when hub/engine/audit are nil -- e.g. an older test-only masterState built
// directly (not via startMaster), or a defensive future caller.
func TestFleetAPIMutationsNilSafeWithoutOptionalFields(t *testing.T) {
	dir := t.TempDir()
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(fleet.Node{ID: nodeID, Name: "n", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(time.Minute))
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	loop := newMasterLoop(reg, tracker, sink, nil, fleetDeps{alert: func(Alert) bool { return true }}, time.Now())
	m := &masterState{reg: reg, tracker: tracker, sink: sink, loop: loop}
	api := fleetAPIFor(m)

	if err := api.RevokeNode(nodeID); err != nil {
		t.Fatal(err)
	}
	if err := api.RenameNode(nodeID, "x"); err != nil {
		t.Fatal(err)
	}
	if err := api.RemoveNode(nodeID); err != nil {
		t.Fatal(err)
	}
}
