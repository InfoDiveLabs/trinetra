package control

import (
	"errors"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type fleetFake struct {
	*fakeAPI
	nodes   map[string]core.API
	renamed string
	removed string
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
func (a fleetFakeAPI) RenameNode(id, name string) error   { a.f.renamed = id + "=" + name; return nil }
func (a fleetFakeAPI) SetNodeTags(string, []string) error { return nil }
func (a fleetFakeAPI) RevokeNode(string) error            { return errors.New("nope") }
func (a fleetFakeAPI) RemoveNode(id string) error         { a.f.removed = id; return nil }
func (a fleetFakeAPI) Tokens() ([]core.TokenView, error) {
	return []core.TokenView{{ID: "t1", Uses: 1}}, nil
}
func (a fleetFakeAPI) CreateToken(s core.TokenSpec) (core.CreatedToken, error) {
	return core.CreatedToken{Token: core.TokenView{ID: "t2", Tags: s.Tags}, JoinCode: "swj1_x"}, nil
}
func (a fleetFakeAPI) DeleteToken(string) error { return nil }

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
	if _, err := c.ForNode("nope").Snapshot(); err == nil || !strings.Contains(err.Error(), "no such fleet node") {
		t.Fatalf("unknown node err = %v", err)
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
	f := &fleetFake{fakeAPI: &fakeAPI{}, nodes: map[string]core.API{}}
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
	if err := fl.RenameNode("n1", "web-01"); err != nil || f.renamed != "n1=web-01" {
		t.Fatalf("rename err %v renamed %q", err, f.renamed)
	}
	if err := fl.RevokeNode("n1"); err == nil || err.Error() != "nope" {
		t.Fatalf("revoke err = %v", err)
	}
	if err := fl.RemoveNode("n1"); err != nil || f.removed != "n1" {
		t.Fatalf("remove err %v removed %q", err, f.removed)
	}
	ct, err := fl.CreateToken(core.TokenSpec{Tags: []string{"lab"}})
	if err != nil || ct.JoinCode != "swj1_x" || ct.Token.Tags[0] != "lab" {
		t.Fatalf("create token %+v err %v", ct, err)
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
