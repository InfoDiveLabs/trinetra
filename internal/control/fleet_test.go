package control

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type fleetFake struct {
	*fakeAPI
	nodes             map[string]core.API
	renamed           string
	renamedActor      string
	removed           string
	removedActor      string
	revokedActor      string
	deletedTokenActor string
	taggedActor       string
	depsID            string
	depsSet           []string
	depsActor         string

	incidentsFilter core.IncidentFilter
	ackedID         string
	ackedActor      string
	explainKey      string
	auditLimit      int

	createdSilence    core.Silence
	expiredSilenceID  string
	expiredActor      string
	expireSilenceErr  error
	savedMaintenance  core.Maintenance
	deletedMaintID    string
	deletedMaintActor string

	setAlertingCfg   core.AlertingConfig
	setAlertingActor string
	routeTestAlert   core.TestAlert
	ruleStates       []core.RuleState

	managed             []core.ManagedFragment
	savedManaged        core.ManagedFragment
	savedManagedActor   string
	deletedManagedID    string
	deletedManagedActor string
	managedStatus       []core.ManagedStatus

	fleetSeries       []core.FleetSeriesPoint
	fleetSeriesMetric string
	fleetSeriesFilter core.NodeFilter
	fleetSeriesAgg    core.Agg
	fleetSeriesFrom   int64
	fleetSeriesTo     int64
	fleetSeriesRes    core.Resolution
}

type fleetFakeAPI struct{ f *fleetFake }

func (f *fleetFake) Fleet() core.FleetAPI { return fleetFakeAPI{f} }
func (f *fleetFake) Node(id string) (core.API, error) {
	if n, ok := f.nodes[id]; ok {
		return n, nil
	}
	return nil, core.ErrNoSuchNode
}

func (a fleetFakeAPI) Status() (core.FleetStatus, error) {
	return core.FleetStatus{Role: "master", Nodes: 2}, nil
}
func (a fleetFakeAPI) Nodes(f core.NodeFilter) ([]core.NodeSummary, error) {
	all := []core.NodeSummary{{ID: "self", Name: "m", Self: true, State: "online"}, {ID: "n1", Name: "web-1", State: "down", Tags: []string{"web"}}}
	var out []core.NodeSummary
	for _, n := range all {
		if f.Match(n) {
			out = append(out, n)
		}
	}
	return out, nil
}
func (a fleetFakeAPI) RenameNode(id, name, actor string) error {
	a.f.renamed, a.f.renamedActor = id+"="+name, actor
	return nil
}
func (a fleetFakeAPI) SetNodeTags(id string, tags []string, actor string) error {
	a.f.taggedActor = actor
	return nil
}
func (a fleetFakeAPI) SetNodeDeps(id string, deps []string, actor string) error {
	a.f.depsID, a.f.depsSet, a.f.depsActor = id, deps, actor
	return nil
}
func (a fleetFakeAPI) RevokeNode(id, actor string) error {
	a.f.revokedActor = actor
	return errors.New("nope")
}
func (a fleetFakeAPI) RemoveNode(id, actor string) error {
	a.f.removed, a.f.removedActor = id, actor
	return nil
}
func (a fleetFakeAPI) Tokens() ([]core.TokenView, error) {
	return []core.TokenView{{ID: "t1", Uses: 1}}, nil
}
func (a fleetFakeAPI) CreateToken(s core.TokenSpec) (core.CreatedToken, error) {
	return core.CreatedToken{Token: core.TokenView{ID: "t2", Tags: s.Tags}, JoinCode: "swj1_x"}, nil
}
func (a fleetFakeAPI) DeleteToken(id, actor string) error {
	a.f.deletedTokenActor = actor
	return nil
}
func (a fleetFakeAPI) Incidents(f core.IncidentFilter) ([]core.Incident, error) {
	a.f.incidentsFilter = f
	return []core.Incident{{ID: "abc123def456", State: f.State, Title: "cpu high"}}, nil
}
func (a fleetFakeAPI) Incident(id string) (core.Incident, error) {
	return core.Incident{ID: id, State: "firing", Title: "cpu high"}, nil
}
func (a fleetFakeAPI) AckIncident(id, actor string) error {
	a.f.ackedID, a.f.ackedActor = id, actor
	return nil
}
func (a fleetFakeAPI) Explain(key string) ([]core.IncidentEvent, error) {
	a.f.explainKey = key
	return []core.IncidentEvent{{TS: 1000, Kind: "fired", Detail: "fired on n1"}}, nil
}
func (a fleetFakeAPI) Audit(limit int) ([]core.AuditEntry, error) {
	a.f.auditLimit = limit
	return []core.AuditEntry{{TS: 1000, Actor: "cli", Action: "revoke_node"}}, nil
}
func (a fleetFakeAPI) Silences() ([]core.Silence, error) {
	return []core.Silence{{ID: "s1", Author: "cli"}}, nil
}
func (a fleetFakeAPI) CreateSilence(s core.Silence) (core.Silence, error) {
	a.f.createdSilence = s
	s.ID = "s2"
	return s, nil
}
func (a fleetFakeAPI) ExpireSilence(id, actor string) error {
	a.f.expiredSilenceID, a.f.expiredActor = id, actor
	return a.f.expireSilenceErr
}
func (a fleetFakeAPI) Maintenances() ([]core.Maintenance, error) {
	return []core.Maintenance{{ID: "m1", Name: "patch window"}}, nil
}
func (a fleetFakeAPI) SaveMaintenance(m core.Maintenance) (core.Maintenance, error) {
	a.f.savedMaintenance = m
	m.ID = "m2"
	return m, nil
}
func (a fleetFakeAPI) DeleteMaintenance(id, actor string) error {
	a.f.deletedMaintID, a.f.deletedMaintActor = id, actor
	return nil
}

func (a fleetFakeAPI) Alerting() (core.AlertingConfig, error) {
	return core.AlertingConfig{DefaultPolicy: "default"}, nil
}

func (a fleetFakeAPI) SetAlerting(cfg core.AlertingConfig, actor string) error {
	a.f.setAlertingCfg, a.f.setAlertingActor = cfg, actor
	return nil
}

func (a fleetFakeAPI) RouteTest(alert core.TestAlert) (core.RouteDecision, error) {
	a.f.routeTestAlert = alert
	return core.RouteDecision{Policies: []core.Policy{{Name: "default"}}}, nil
}

func (a fleetFakeAPI) RuleStates() ([]core.RuleState, error) {
	return a.f.ruleStates, nil
}

func (a fleetFakeAPI) Managed() ([]core.ManagedFragment, error) { return a.f.managed, nil }

func (a fleetFakeAPI) SaveManaged(frag core.ManagedFragment, actor string) (core.ManagedFragment, error) {
	a.f.savedManaged, a.f.savedManagedActor = frag, actor
	if frag.ID == "" {
		frag.ID = "mf2"
	}
	return frag, nil
}

func (a fleetFakeAPI) DeleteManaged(id, actor string) error {
	a.f.deletedManagedID, a.f.deletedManagedActor = id, actor
	return nil
}

func (a fleetFakeAPI) ManagedStatus() ([]core.ManagedStatus, error) {
	return a.f.managedStatus, nil
}

func (a fleetFakeAPI) FleetSeries(metric string, filter core.NodeFilter, agg core.Agg, from, to int64, res core.Resolution) ([]core.FleetSeriesPoint, error) {
	a.f.fleetSeriesMetric, a.f.fleetSeriesFilter, a.f.fleetSeriesAgg = metric, filter, agg
	a.f.fleetSeriesFrom, a.f.fleetSeriesTo, a.f.fleetSeriesRes = from, to, res
	return a.f.fleetSeries, nil
}

func TestClientRoutesToNode(t *testing.T) {
	remote := &fakeAPI{snapshot: core.DashboardView{CPU: 77}}
	f := &fleetFake{fakeAPI: &fakeAPI{snapshot: core.DashboardView{CPU: 11}}, nodes: map[string]core.API{"n1": remote}}
	c, err := Dial(startTestServer(t, f, "tok"), "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	local, _ := c.Snapshot()
	n1, err := c.Node("n1")
	if err != nil {
		t.Fatal(err)
	}
	rv, err := n1.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if local.CPU != 11 || rv.CPU != 77 {
		t.Fatalf("local=%v remote=%v", local.CPU, rv.CPU)
	}
	// errors.Is against core.ErrNoSuchNode must survive the wire (reconstructWireErr),
	// not just the message text.
	if _, err := c.ForNode("nope").Snapshot(); err == nil || !strings.Contains(err.Error(), "no such fleet node") {
		t.Fatalf("unknown node err = %v", err)
	} else if !errors.Is(err, core.ErrNoSuchNode) {
		t.Errorf("unknown node err = %v, want errors.Is(err, core.ErrNoSuchNode) after a real control-socket round trip", err)
	}
	self, _ := c.ForNode(core.SelfNodeID).Snapshot()
	if self.CPU != 11 {
		t.Fatalf("self view cpu = %v", self.CPU)
	}
	if err := n1.(*Client).Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(); err != nil {
		t.Fatalf("closing a node view closed the shared connection: %v", err)
	}
}

func TestClientFleetMethods(t *testing.T) {
	f := &fleetFake{fakeAPI: &fakeAPI{}, nodes: map[string]core.API{},
		ruleStates: []core.RuleState{{Name: "hot-web", Expr: "count(tag:web, cpu > 90) >= 1 for 5m", Value: 1, HasValue: true, Firing: true}}}
	c, _ := Dial(startTestServer(t, f, "tok"), "tok")
	defer c.Close()
	fl := c.Fleet()
	st, err := fl.Status()
	if err != nil || st.Role != "master" || st.Nodes != 2 {
		t.Fatalf("status %+v err %v", st, err)
	}
	ns, _ := fl.Nodes(core.NodeFilter{State: "down"})
	if len(ns) != 1 || ns[0].ID != "n1" {
		t.Fatalf("nodes = %+v", ns)
	}
	if err := fl.RenameNode("n1", "web-01", "root"); err != nil || f.renamed != "n1=web-01" || f.renamedActor != "root" {
		t.Fatalf("rename err %v renamed %q actor %q", err, f.renamed, f.renamedActor)
	}
	if err := fl.SetNodeDeps("n1", []string{"n2", "tag:db"}, "cli"); err != nil ||
		f.depsID != "n1" || strings.Join(f.depsSet, ",") != "n2,tag:db" || f.depsActor != "cli" {
		t.Fatalf("set node deps err %v id %q deps %v actor %q", err, f.depsID, f.depsSet, f.depsActor)
	}
	if err := fl.RevokeNode("n1", "root"); err == nil || err.Error() != "nope" || f.revokedActor != "root" {
		t.Fatalf("revoke err = %v actor %q", err, f.revokedActor)
	}
	if err := fl.RemoveNode("n1", "root"); err != nil || f.removed != "n1" || f.removedActor != "root" {
		t.Fatalf("remove err %v removed %q actor %q", err, f.removed, f.removedActor)
	}
	ct, err := fl.CreateToken(core.TokenSpec{Tags: []string{"lab"}})
	if err != nil || ct.JoinCode != "swj1_x" || ct.Token.Tags[0] != "lab" {
		t.Fatalf("create token %+v err %v", ct, err)
	}
	if err := fl.DeleteToken("t2", "root"); err != nil || f.deletedTokenActor != "root" {
		t.Fatalf("delete token err %v actor %q", err, f.deletedTokenActor)
	}

	incs, err := fl.Incidents(core.IncidentFilter{State: "firing"})
	if err != nil || len(incs) != 1 || incs[0].ID != "abc123def456" || f.incidentsFilter.State != "firing" {
		t.Fatalf("incidents = %+v err %v filter %+v", incs, err, f.incidentsFilter)
	}
	inc, err := fl.Incident("abc123def456")
	if err != nil || inc.ID != "abc123def456" || inc.State != "firing" {
		t.Fatalf("incident = %+v err %v", inc, err)
	}
	if err := fl.AckIncident("abc123def456", "cli"); err != nil || f.ackedID != "abc123def456" || f.ackedActor != "cli" {
		t.Fatalf("ack err %v id %q actor %q", err, f.ackedID, f.ackedActor)
	}
	events, err := fl.Explain("cpu")
	if err != nil || len(events) != 1 || events[0].Kind != "fired" || f.explainKey != "cpu" {
		t.Fatalf("explain = %+v err %v", events, err)
	}
	audit, err := fl.Audit(5)
	if err != nil || len(audit) != 1 || audit[0].Action != "revoke_node" || f.auditLimit != 5 {
		t.Fatalf("audit = %+v err %v", audit, err)
	}
	rs, err := fl.RuleStates()
	if err != nil || len(rs) != 1 || rs[0].Name != "hot-web" || !rs[0].Firing {
		t.Fatalf("rule states = %+v err %v", rs, err)
	}

	f.fleetSeries = []core.FleetSeriesPoint{{Node: "web-01", TS: 1000, Value: 42}}
	fs, err := fl.FleetSeries("cpu", core.NodeFilter{Tag: "web"}, core.AggNone, 500, 1500, core.ResRaw)
	if err != nil || len(fs) != 1 || fs[0].Node != "web-01" || fs[0].Value != 42 {
		t.Fatalf("fleet series = %+v err %v", fs, err)
	}
	if f.fleetSeriesMetric != "cpu" || f.fleetSeriesFilter.Tag != "web" || f.fleetSeriesAgg != core.AggNone ||
		f.fleetSeriesFrom != 500 || f.fleetSeriesTo != 1500 || f.fleetSeriesRes != core.ResRaw {
		t.Fatalf("fleet series args not carried over the wire: metric=%q filter=%+v agg=%q from=%d to=%d res=%v",
			f.fleetSeriesMetric, f.fleetSeriesFilter, f.fleetSeriesAgg, f.fleetSeriesFrom, f.fleetSeriesTo, f.fleetSeriesRes)
	}
}

// TestClientPreservesErrNotFoundOverTheWire pins that a Fleet.* error wrapping
// core.ErrNotFound still satisfies errors.Is(err, core.ErrNotFound) across a
// real control-socket round trip; internal/web maps it to 404.
func TestClientPreservesErrNotFoundOverTheWire(t *testing.T) {
	f := &fleetFake{fakeAPI: &fakeAPI{}, expireSilenceErr: fmt.Errorf("no such silence %q: %w", "sil1", core.ErrNotFound)}
	c, _ := Dial(startTestServer(t, f, "tok"), "tok")
	defer c.Close()

	err := c.Fleet().ExpireSilence("sil1", "root")
	if err == nil {
		t.Fatal("want an error expiring an unknown silence")
	}
	if !strings.Contains(err.Error(), "no such silence") {
		t.Errorf("err = %v, want the original message text preserved verbatim", err)
	}
	if !errors.Is(err, core.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(err, core.ErrNotFound) after a real control-socket round trip", err)
	}
}

func TestFleetMethodsOnNonFleetDaemon(t *testing.T) {
	c, _ := Dial(startTestServer(t, &fakeAPI{}, "tok"), "tok")
	defer c.Close()
	if _, err := c.Fleet().Status(); err == nil {
		t.Fatal("fleet call on a non-fleet API succeeded")
	}
	if _, err := c.ForNode("n1").Snapshot(); err == nil {
		t.Fatal("node routing on a non-fleet API succeeded")
	}
}
