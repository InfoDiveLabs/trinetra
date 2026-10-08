package control

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// spCall records the arguments of one StatusPage call as the server saw them.
type spCall struct {
	method   string
	id       string
	updateID string
	actor    string
	title    string
	services []string
	include  bool
	service  core.StatusService
	incident core.NewIncident
	update   core.NewUpdate
}

type fakeStatusPage struct {
	calls    []spCall
	svcs     []core.StatusService
	childErr bool
}

func (f *fakeStatusPage) rec(c spCall) { f.calls = append(f.calls, c) }

func (f *fakeStatusPage) last() spCall { return f.calls[len(f.calls)-1] }

func (f *fakeStatusPage) Services() ([]core.StatusService, error) {
	f.rec(spCall{method: "Services"})
	if f.childErr {
		return nil, core.ErrStatusPageOnChild
	}
	return f.svcs, nil
}

func (f *fakeStatusPage) SetService(s core.StatusService, actor string) (core.StatusService, error) {
	f.rec(spCall{method: "SetService", service: s, actor: actor})
	s.Name += "!"
	return s, nil
}

func (f *fakeStatusPage) DeleteService(id, actor string) error {
	f.rec(spCall{method: "DeleteService", id: id, actor: actor})
	return nil
}

func (f *fakeStatusPage) Evaluation() ([]core.ServiceEvaluation, error) {
	f.rec(spCall{method: "Evaluation"})
	return []core.ServiceEvaluation{{ServiceID: "api", State: core.StateDegraded, Computed: core.StateOutage, PendingSince: 7, Reason: "r", Missing: []string{"x"}}}, nil
}

func (f *fakeStatusPage) Incidents(includeResolved bool) ([]core.StatusIncident, error) {
	f.rec(spCall{method: "Incidents", include: includeResolved})
	n := "open"
	if includeResolved {
		n = "all"
	}
	return []core.StatusIncident{{ID: n}}, nil
}

func (f *fakeStatusPage) Incident(id string) (core.StatusIncident, error) {
	f.rec(spCall{method: "Incident", id: id})
	return core.StatusIncident{ID: id, Title: "t", Impact: core.StateOutage, Services: []string{"a"}}, nil
}

func (f *fakeStatusPage) CreateIncident(in core.NewIncident, actor string) (core.StatusIncident, error) {
	f.rec(spCall{method: "CreateIncident", incident: in, actor: actor})
	return core.StatusIncident{ID: "new", Title: in.Title, Services: in.Services, Impact: in.Impact, Status: in.Update.Status}, nil
}

func (f *fakeStatusPage) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	f.rec(spCall{method: "PostUpdate", id: id, update: u, actor: actor})
	if id == "missing" {
		return core.StatusIncident{}, core.ErrNotFound
	}
	return core.StatusIncident{ID: id, Status: u.Status}, nil
}

func (f *fakeStatusPage) EditUpdate(id, updateID string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	f.rec(spCall{method: "EditUpdate", id: id, updateID: updateID, update: u, actor: actor})
	return core.StatusIncident{ID: id, Updates: []core.IncidentUpdate{{ID: updateID, Status: u.Status, Message: u.Message}}}, nil
}

func (f *fakeStatusPage) EditIncident(id, title string, services []string, actor string) (core.StatusIncident, error) {
	f.rec(spCall{method: "EditIncident", id: id, title: title, services: services, actor: actor})
	return core.StatusIncident{ID: id, Title: title, Services: services}, nil
}

func (f *fakeStatusPage) DeleteIncident(id, actor string) error {
	f.rec(spCall{method: "DeleteIncident", id: id, actor: actor})
	return nil
}

func (f *fakeStatusPage) Public() (core.PublicStatus, error) {
	f.rec(spCall{method: "Public"})
	return core.PublicStatus{Schema: 1, Title: "T", Overall: core.OverallPartial}, nil
}

type statusPageServed struct {
	*fakeAPI
	sp core.StatusPageAPI
}

func (s statusPageServed) StatusPage() core.StatusPageAPI { return s.sp }

func dialStatusPage(t *testing.T, fake core.API) *Client {
	t.Helper()
	c, err := Dial(startTestServer(t, fake, "tok"), "tok")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStatusPageAllMethodsOverWire(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API"}}}
	c := dialStatusPage(t, statusPageServed{fakeAPI: &fakeAPI{}, sp: sp})
	api := c.StatusPage()

	// expect asserts the server saw exactly want (method + args) for the last call.
	expect := func(want spCall) {
		t.Helper()
		if got := sp.last(); !reflect.DeepEqual(got, want) {
			t.Fatalf("server saw\n %+v\nwant\n %+v", got, want)
		}
	}

	svcs, err := api.Services()
	if err != nil || !reflect.DeepEqual(svcs, sp.svcs) {
		t.Fatalf("Services: %v %+v", err, svcs)
	}
	expect(spCall{method: "Services"})

	in := core.StatusService{
		ID: "db", Name: "DB", Description: "d", Group: "core", Order: 3, HoldDownSec: 90,
		Targets: []core.StatusTarget{{Kind: core.TargetContainer, Node: "n1", Value: "pg"}},
	}
	out, err := api.SetService(in, "alice")
	want := in
	want.Name = "DB!"
	if err != nil || !reflect.DeepEqual(out, want) {
		t.Fatalf("SetService: %v %+v", err, out)
	}
	expect(spCall{method: "SetService", service: in, actor: "alice"})

	if err := api.DeleteService("db", "bob"); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	expect(spCall{method: "DeleteService", id: "db", actor: "bob"})

	ev, err := api.Evaluation()
	wantEv := []core.ServiceEvaluation{{ServiceID: "api", State: core.StateDegraded, Computed: core.StateOutage, PendingSince: 7, Reason: "r", Missing: []string{"x"}}}
	if err != nil || !reflect.DeepEqual(ev, wantEv) {
		t.Fatalf("Evaluation: %v %+v", err, ev)
	}
	expect(spCall{method: "Evaluation"})

	for _, include := range []bool{true, false} {
		incs, err := api.Incidents(include)
		wantID := map[bool]string{true: "all", false: "open"}[include]
		if err != nil || len(incs) != 1 || incs[0].ID != wantID {
			t.Fatalf("Incidents(%v): %v %+v", include, err, incs)
		}
		expect(spCall{method: "Incidents", include: include})
	}

	inc, err := api.Incident("i9")
	if err != nil || inc.ID != "i9" || inc.Title != "t" || inc.Impact != core.StateOutage || !reflect.DeepEqual(inc.Services, []string{"a"}) {
		t.Fatalf("Incident: %v %+v", err, inc)
	}
	expect(spCall{method: "Incident", id: "i9"})

	ni := core.NewIncident{
		Title: "Down", Services: []string{"api", "db"}, Impact: core.StateOutage,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "looking"},
	}
	inc, err = api.CreateIncident(ni, "carol")
	if err != nil || inc.ID != "new" || inc.Title != "Down" || inc.Impact != core.StateOutage ||
		inc.Status != core.IncidentInvestigating || !reflect.DeepEqual(inc.Services, ni.Services) {
		t.Fatalf("CreateIncident: %v %+v", err, inc)
	}
	expect(spCall{method: "CreateIncident", incident: ni, actor: "carol"})

	nu := core.NewUpdate{Status: core.IncidentIdentified, Message: "found it"}
	inc, err = api.PostUpdate("i1", nu, "alice")
	if err != nil || inc.ID != "i1" || inc.Status != core.IncidentIdentified {
		t.Fatalf("PostUpdate: %v %+v", err, inc)
	}
	expect(spCall{method: "PostUpdate", id: "i1", update: nu, actor: "alice"})

	eu := core.NewUpdate{Status: core.IncidentMonitoring, Message: "edited"}
	inc, err = api.EditUpdate("i1", "u7", eu, "dave")
	if err != nil || inc.ID != "i1" || len(inc.Updates) != 1 || inc.Updates[0].ID != "u7" ||
		inc.Updates[0].Status != eu.Status || inc.Updates[0].Message != eu.Message {
		t.Fatalf("EditUpdate: %v %+v", err, inc)
	}
	expect(spCall{method: "EditUpdate", id: "i1", updateID: "u7", update: eu, actor: "dave"})

	inc, err = api.EditIncident("i2", "New title", []string{"x", "y"}, "erin")
	if err != nil || inc.ID != "i2" || inc.Title != "New title" || !reflect.DeepEqual(inc.Services, []string{"x", "y"}) {
		t.Fatalf("EditIncident: %v %+v", err, inc)
	}
	expect(spCall{method: "EditIncident", id: "i2", title: "New title", services: []string{"x", "y"}, actor: "erin"})

	if err := api.DeleteIncident("i3", "frank"); err != nil {
		t.Fatalf("DeleteIncident: %v", err)
	}
	expect(spCall{method: "DeleteIncident", id: "i3", actor: "frank"})

	pub, err := api.Public()
	if err != nil || pub.Schema != 1 || pub.Title != "T" || pub.Overall != core.OverallPartial {
		t.Fatalf("Public: %v %+v", err, pub)
	}
	expect(spCall{method: "Public"})

	if len(sp.calls) != 13 { // 12 methods, Incidents twice
		t.Fatalf("server saw %d calls, want 13", len(sp.calls))
	}
}

func TestStatusPageErrorsSurviveWire(t *testing.T) {
	sp := &fakeStatusPage{}
	c := dialStatusPage(t, statusPageServed{fakeAPI: &fakeAPI{}, sp: sp})
	if _, err := c.StatusPage().PostUpdate("missing", core.NewUpdate{Status: "identified", Message: "m"}, "a"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ErrNotFound lost over the wire: %v", err)
	}
	sp.childErr = true
	if _, err := c.StatusPage().Services(); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Fatalf("child sentinel lost over the wire: %v", err)
	}
}

type nilStatusPageServed struct{ *fakeAPI }

func (nilStatusPageServed) StatusPage() core.StatusPageAPI { return nil }

func TestStatusPageUnsupportedServer(t *testing.T) {
	for name, fake := range map[string]core.API{
		"no provider":    &fakeAPI{},
		"nil status API": nilStatusPageServed{&fakeAPI{}},
	} {
		t.Run(name, func(t *testing.T) {
			c := dialStatusPage(t, fake)
			_, err := c.StatusPage().Services()
			if err == nil || !strings.Contains(err.Error(), "status page not available") {
				t.Fatalf("want 'status page not available' error, got %v", err)
			}
		})
	}
}
