package trinetra

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type imHarness struct {
	m      *incidentManager
	now    time.Time
	echoes []string
}

func newIMHarness(t *testing.T, svcIDs ...string) *imHarness {
	h := &imHarness{now: evalT0}
	d := &statusPageData{State: map[string]*serviceRuntimeState{}}
	for _, id := range svcIDs {
		d.Services = append(d.Services, core.StatusService{ID: id, Name: strings.ToUpper(id)})
		d.State[id] = &serviceRuntimeState{State: core.StateOperational}
	}
	n := 0
	h.m = &incidentManager{
		data:        d,
		now:         func() time.Time { return h.now },
		autoResolve: func() time.Duration { return 24 * time.Hour },
		echo: func(inc core.StatusIncident, u core.IncidentUpdate) {
			h.echoes = append(h.echoes, inc.Title+"|"+u.Status)
		},
		newID: func() string { n++; return fmt.Sprintf("id%d", n) },
	}
	return h
}

func (h *imHarness) setState(id string, s core.ServiceState) { h.m.data.State[id].State = s }

func TestAutoOpenFoldsServicesInOneTick(t *testing.T) {
	h := newIMHarness(t, "api", "web")
	h.setState("api", core.StateOutage)
	h.setState("web", core.StateDegraded)
	h.m.applyChanges([]stateChange{
		{ServiceID: "api", From: core.StateOperational, To: core.StateOutage, Reason: "node down"},
		{ServiceID: "web", From: core.StateOperational, To: core.StateDegraded, Reason: "warning"},
	})
	incs := h.m.data.Incidents
	if len(incs) != 1 {
		t.Fatalf("want 1 folded incident, got %d", len(incs))
	}
	inc := incs[0]
	if !inc.Auto || inc.Impact != core.StateOutage || inc.Status != core.IncidentInvestigating {
		t.Fatalf("incident %+v", inc)
	}
	if inc.Title != "Outage: API, WEB" {
		t.Errorf("title %q", inc.Title)
	}
	if len(inc.Updates) != 1 || inc.Updates[0].Author != core.SystemAuthor {
		t.Fatalf("updates %+v", inc.Updates)
	}
	if strings.Contains(inc.Updates[0].Message, "node down") {
		t.Fatal("internal reason leaked into the public message")
	}
	if len(inc.Triggers) != 2 {
		t.Errorf("triggers %v", inc.Triggers)
	}
	if len(h.echoes) != 1 {
		t.Errorf("echoes %v", h.echoes)
	}
}

func TestAutoOpenReusesOpenIncidentAndRaisesImpact(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateDegraded)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateDegraded}})
	h.now = h.now.Add(time.Minute)
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateDegraded, To: core.StateOutage}})
	if len(h.m.data.Incidents) != 1 {
		t.Fatalf("opened a second incident: %d", len(h.m.data.Incidents))
	}
	inc := h.m.data.Incidents[0]
	if inc.Impact != core.StateOutage || len(inc.Updates) != 2 {
		t.Fatalf("impact %q updates %d", inc.Impact, len(inc.Updates))
	}
}

func TestRecoveryMovesToMonitoringThenAutoResolves(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.now = h.now.Add(10 * time.Minute)
	h.setState("api", core.StateOperational)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	inc := h.m.data.Incidents[0]
	if inc.Status != core.IncidentMonitoring || inc.RecoveredAt == 0 {
		t.Fatalf("after recovery %+v", inc)
	}
	h.now = h.now.Add(23 * time.Hour)
	if h.m.tickAutoResolve() {
		t.Fatal("resolved before 24h")
	}
	h.now = h.now.Add(time.Hour + time.Second)
	if !h.m.tickAutoResolve() {
		t.Fatal("did not auto-resolve after 24h")
	}
	inc = h.m.data.Incidents[0]
	if inc.Status != core.IncidentResolved || inc.Resolved == 0 {
		t.Fatalf("not resolved: %+v", inc)
	}
}

func TestPersonStatusAfterRecoveryStands(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.setState("api", core.StateOperational)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	id := h.m.data.Incidents[0].ID
	if _, err := h.m.post(id, core.NewUpdate{Status: core.IncidentIdentified, Message: "root cause: db"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := h.m.data.Incidents[0].Status; got != core.IncidentIdentified {
		t.Fatalf("status %q, want identified to stand", got)
	}
}

func TestAutoResolveZeroMeansNever(t *testing.T) {
	h := newIMHarness(t, "api")
	h.m.autoResolve = func() time.Duration { return 0 }
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.setState("api", core.StateOperational)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	h.now = h.now.Add(1000 * time.Hour)
	if h.m.tickAutoResolve() {
		t.Fatal("auto-resolved with auto_resolve_after=0")
	}
}

func TestRefailWhileMonitoringReopensSameIncident(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.setState("api", core.StateOperational)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	if len(h.m.data.Incidents) != 1 {
		t.Fatalf("got %d incidents", len(h.m.data.Incidents))
	}
	inc := h.m.data.Incidents[0]
	if inc.Status != core.IncidentInvestigating || inc.RecoveredAt != 0 {
		t.Fatalf("%+v", inc)
	}
}

func TestMaintenanceTransitionsOpenNoIncident(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateMaintenance)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateMaintenance}})
	if len(h.m.data.Incidents) != 0 {
		t.Fatal("maintenance opened an incident")
	}
}

func TestManualCreatePostEditDelete(t *testing.T) {
	h := newIMHarness(t, "api")
	inc, err := h.m.create(core.NewIncident{Title: "Payments delayed", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "Looking into it"}}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if inc.Auto || inc.Updates[0].Author != "alice" {
		t.Fatalf("%+v", inc)
	}
	if _, err := h.m.create(core.NewIncident{Title: "x", Services: []string{"nope"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "m"}}, "alice"); err == nil {
		t.Fatal("unknown service accepted")
	}
	h.now = h.now.Add(time.Minute)
	inc, err = h.m.editUpdate(inc.ID, inc.Updates[0].ID, core.NewUpdate{Status: core.IncidentInvestigating, Message: "Looking into it now"}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.Updates[0].Edits) != 1 || inc.Updates[0].Edits[0].Message != "Looking into it" || inc.Updates[0].Edits[0].Editor != "bob" {
		t.Fatalf("edit history %+v", inc.Updates[0].Edits)
	}
	inc, err = h.m.post(inc.ID, core.NewUpdate{Status: core.IncidentResolved, Message: "Fixed"}, "alice")
	if err != nil || inc.Status != core.IncidentResolved || inc.Resolved == 0 {
		t.Fatalf("resolve: %v %+v", err, inc)
	}
	if err := h.m.delete(inc.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.m.delete(inc.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPostToUnknownIncidentIsNotFound(t *testing.T) {
	h := newIMHarness(t, "api")
	_, err := h.m.post("nope", core.NewUpdate{Status: core.IncidentIdentified, Message: "x"}, "a")
	if !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}
