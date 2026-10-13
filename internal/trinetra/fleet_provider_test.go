package trinetra

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// newTestMasterState builds a masterState with a real registry, engine, incident store,
// audit log and Hub (no live connections).
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
	loop := newMasterLoop(reg, tracker, sink, engine, fleetDeps{alert: func(Alert) {}}, time.Now())
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
		if e.Action == "fleet.incident.ack" && e.Actor == "cli" && e.Target == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit missing fleet.incident.ack entry: %+v", entries)
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
		// AckIncident must not panic pushing to a self/master alert (no node) or to an
		// already-resolved one; both should simply be skipped.
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

	// Actor plumbing: each call records whatever actor the caller passes (here a real per-call
	// actor, like a signed-in web user's name), not a placeholder "unknown".
	if err := api.RenameNode(nodeID, "web1-renamed", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := api.SetNodeTags(nodeID, []string{"prod"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := api.RevokeNode(nodeID, "alice"); err != nil {
		t.Fatal(err)
	}
	tok, err := api.CreateToken(core.TokenSpec{Creator: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.DeleteToken(tok.Token.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	secondNode, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: secondNode, Name: "gone", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := api.RemoveNode(secondNode, "alice"); err != nil {
		t.Fatal(err)
	}

	entries, err := api.Audit(0)
	if err != nil {
		t.Fatal(err)
	}
	wantActions := map[string]string{
		"fleet.node.rename": "alice", "fleet.node.tags": "alice", "fleet.node.revoke": "alice",
		"fleet.token.create": "cli", "fleet.token.delete": "alice", "fleet.node.remove": "alice",
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

// TestRenameNodeRejectsNameAlreadyUsed: renaming a node to a name already used
// (case-insensitively) by ANOTHER node is refused with the exact error text.
func TestRenameNodeRejectsNameAlreadyUsed(t *testing.T) {
	m := newTestMasterState(t)
	id1, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: id1, Name: "web1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: id2, Name: "db1", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)

	err = api.RenameNode(id2, "WEB1", "alice")
	if err == nil {
		t.Fatal("rename to an already-used name (case-insensitive) must be refused")
	}
	wantErr := fmt.Sprintf("name %q is already used by node %s", "WEB1", fleet.ShortNodeID(id1))
	if err.Error() != wantErr {
		t.Fatalf("err = %q, want %q", err.Error(), wantErr)
	}
	if n, _ := m.reg.Get(id2); n.Name != "db1" {
		t.Fatalf("node kept its original name after a refused rename, got %q", n.Name)
	}

	// Renaming a node to ITS OWN current name (even different case) is not a
	// conflict with itself.
	if err := api.RenameNode(id1, "WEB1", "alice"); err != nil {
		t.Fatalf("renaming to a case-variant of its own current name should succeed: %v", err)
	}
}

// TestFleetAPIMutationsNilSafeWithoutOptionalFields guards against a panic when
// hub/engine/audit are nil -- e.g. an older test-only masterState built directly.
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
	loop := newMasterLoop(reg, tracker, sink, nil, fleetDeps{alert: func(Alert) {}}, time.Now())
	m := &masterState{reg: reg, tracker: tracker, sink: sink, loop: loop}
	api := fleetAPIFor(m)

	if err := api.RevokeNode(nodeID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := api.RenameNode(nodeID, "x", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := api.RemoveNode(nodeID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.Alerting(); err != nil {
		t.Fatalf("Alerting() with no alerting store must return the built-in default, not error: %v", err)
	}
	if _, err := api.RouteTest(core.TestAlert{Rule: "cpu"}); err != nil {
		t.Fatalf("RouteTest() with no silences/registry must not error: %v", err)
	}
}

// TestFleetAPIAlertingShowApplyRoundTrip: Alerting() reports the built-in default until
// SetAlerting saves something.
func TestFleetAPIAlertingShowApplyRoundTrip(t *testing.T) {
	m := newTestMasterState(t)
	dir := t.TempDir()
	alerting, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.alerting = alerting
	m.getCfg = func() *config.Config {
		return &config.Config{Channels: []config.ChannelConfig{{Name: "slack", Type: "webhook", Enabled: true}}}
	}
	api := fleetAPIFor(m)

	got, err := api.Alerting()
	if err != nil || got.Version != 0 || got.DefaultPolicy != "default" {
		t.Fatalf("Alerting() before any save = %+v err %v, want the built-in default", got, err)
	}

	bad := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"bogus"}}}}},
		DefaultPolicy: "p",
	}
	if err := api.SetAlerting(bad, "cli"); err == nil {
		t.Fatal("SetAlerting with an unknown channel name must fail")
	}

	good := core.AlertingConfig{
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}}},
		DefaultPolicy: "p",
	}
	if err := api.SetAlerting(good, "cli-tester"); err != nil {
		t.Fatalf("SetAlerting with a valid config: %v", err)
	}
	got, err = api.Alerting()
	if err != nil || got.Version != 1 || got.DefaultPolicy != "p" {
		t.Fatalf("Alerting() after save = %+v err %v", got, err)
	}

	entries, err := api.Audit(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "fleet.alerting.set" && e.Actor == "cli-tester" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit log missing fleet.alerting.set: %+v", entries)
	}
}

// TestFleetAPIRouteTestMatchesResolveRoute is the test-enforced invariant RouteTest must.
func TestFleetAPIRouteTestMatchesResolveRoute(t *testing.T) {
	m := newTestMasterState(t)
	dir := t.TempDir()
	alerting, err := loadAlertingStore(filepath.Join(dir, "alerting.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := core.AlertingConfig{
		Routes:        []core.Route{{Name: "web", Matchers: []core.Matcher{{Node: "web*"}}, Policy: "p"}},
		Policies:      []core.Policy{{Name: "p", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"slack"}}}}},
		DefaultPolicy: "p",
	}
	if _, err := alerting.Set(cfg, allChannelsValid); err != nil {
		t.Fatal(err)
	}
	m.alerting = alerting
	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: nodeID, Name: "web1", Tags: []string{"prod"}, Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	api := fleetAPIFor(m)

	got, err := api.RouteTest(core.TestAlert{Node: "web1", Rule: "cpu", Severity: "critical"})
	if err != nil {
		t.Fatal(err)
	}
	want := resolveRoute(alerting.Get(), nodeID, "web1", []string{"prod"}, "cpu", "critical")
	if got.Route != want.Route || len(got.Policies) != len(want.Policies) {
		t.Fatalf("RouteTest = %+v, want route=%q policies=%+v matching resolveRoute directly", got, want.Route, want.Policies)
	}
	for i, p := range want.Policies {
		if got.Policies[i].Name != p.Name {
			t.Fatalf("RouteTest.Policies[%d] = %q, want %q", i, got.Policies[i].Name, p.Name)
		}
	}
}
