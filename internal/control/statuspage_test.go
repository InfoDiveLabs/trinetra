// internal/control/statuspage_test.go
package control

import (
	"errors"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type fakeStatusPage struct {
	core.StatusPageAPI // nil: unimplemented methods panic, so tests only call the overridden ones
	svcs               []core.StatusService
	posted             core.NewUpdate
	actor              string
	childErr           bool
}

func (f *fakeStatusPage) Services() ([]core.StatusService, error) {
	if f.childErr {
		return nil, core.ErrStatusPageOnChild
	}
	return f.svcs, nil
}

func (f *fakeStatusPage) PostUpdate(id string, u core.NewUpdate, actor string) (core.StatusIncident, error) {
	if id == "missing" {
		return core.StatusIncident{}, core.ErrNotFound
	}
	f.posted, f.actor = u, actor
	return core.StatusIncident{ID: id, Status: u.Status}, nil
}

func (f *fakeStatusPage) Public() (core.PublicStatus, error) {
	return core.PublicStatus{Schema: 1, Overall: core.OverallPartial}, nil
}

type statusPageServed struct {
	*fakeAPI
	sp *fakeStatusPage
}

func (s statusPageServed) StatusPage() core.StatusPageAPI { return s.sp }

func TestStatusPageRoundTrip(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API"}}}
	c, _ := Dial(startTestServer(t, statusPageServed{fakeAPI: &fakeAPI{}, sp: sp}, "tok"), "tok")
	defer c.Close()
	got, err := c.StatusPage().Services()
	if err != nil || len(got) != 1 || got[0].Name != "API" {
		t.Fatalf("Services: %v %+v", err, got)
	}
	inc, err := c.StatusPage().PostUpdate("i1", core.NewUpdate{Status: core.IncidentIdentified, Message: "m"}, "alice")
	if err != nil || inc.Status != core.IncidentIdentified || sp.actor != "alice" {
		t.Fatalf("PostUpdate: %v %+v actor=%q", err, inc, sp.actor)
	}
	if _, err := c.StatusPage().PostUpdate("missing", core.NewUpdate{Status: "identified", Message: "m"}, "a"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("ErrNotFound lost over the wire: %v", err)
	}
	pub, err := c.StatusPage().Public()
	if err != nil || pub.Overall != core.OverallPartial {
		t.Fatalf("Public: %v %+v", err, pub)
	}
	sp.childErr = true
	if _, err := c.StatusPage().Services(); !errors.Is(err, core.ErrStatusPageOnChild) {
		t.Fatalf("child sentinel lost over the wire: %v", err)
	}
}

func TestStatusPageUnsupportedServer(t *testing.T) {
	c, _ := Dial(startTestServer(t, &fakeAPI{}, "tok"), "tok")
	defer c.Close()
	if _, err := c.StatusPage().Services(); err == nil {
		t.Fatal("want error when the served API has no status page")
	}
}
