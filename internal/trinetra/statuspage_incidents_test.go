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
	if u := inc.Updates[1]; u.Status != core.IncidentInvestigating || u.Message != "We're now seeing an outage affecting API." {
		t.Fatalf("update %+v", u)
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
	if u := inc.Updates[len(inc.Updates)-1]; len(inc.Updates) != 3 || u.Status != core.IncidentInvestigating || u.Message != "API is affected again. We're investigating." {
		t.Fatalf("updates %+v", inc.Updates)
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

func openAuto(h *imHarness, ids ...string) {
	var ch []stateChange
	for _, id := range ids {
		h.setState(id, core.StateOutage)
		ch = append(ch, stateChange{ServiceID: id, From: core.StateOperational, To: core.StateOutage, Reason: "r"})
	}
	h.m.applyChanges(ch)
}

func recover1(h *imHarness, id string) bool {
	h.setState(id, core.StateOperational)
	return h.m.applyChanges([]stateChange{{ServiceID: id, From: core.StateOutage, To: core.StateOperational}})
}

func TestIncidentMonitoringOnlyWhenAllServicesRecover(t *testing.T) {
	h := newIMHarness(t, "api", "web")
	openAuto(h, "api", "web")
	recover1(h, "api")
	inc := h.m.data.Incidents[0]
	if inc.Status != core.IncidentInvestigating || inc.RecoveredAt != 0 || len(inc.Updates) != 1 {
		t.Fatalf("posted monitoring after partial recovery: %+v", inc)
	}
	recover1(h, "web")
	inc = h.m.data.Incidents[0]
	if inc.Status != core.IncidentMonitoring || inc.RecoveredAt == 0 || len(inc.Updates) != 2 {
		t.Fatalf("no monitoring after full recovery: %+v", inc)
	}
	if got := inc.Updates[1].Message; got != "API, WEB have recovered. We're monitoring." {
		t.Errorf("message %q", got)
	}
}

func TestIncidentPersonResolvedThenRefailOpensNew(t *testing.T) {
	h := newIMHarness(t, "api")
	openAuto(h, "api")
	id := h.m.data.Incidents[0].ID
	if _, err := h.m.post(id, core.NewUpdate{Status: core.IncidentResolved, Message: "done"}, "alice"); err != nil {
		t.Fatal(err)
	}
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	if len(h.m.data.Incidents) != 2 || h.m.data.Incidents[1].Status != core.IncidentInvestigating {
		t.Fatalf("incidents %+v", h.m.data.Incidents)
	}
}

func TestIncidentReopenedIsNotAutoResolved(t *testing.T) {
	h := newIMHarness(t, "api")
	openAuto(h, "api")
	recover1(h, "api")
	id := h.m.data.Incidents[0].ID
	if _, err := h.m.post(id, core.NewUpdate{Status: core.IncidentResolved, Message: "done"}, "alice"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	inc, err := h.m.post(id, core.NewUpdate{Status: core.IncidentInvestigating, Message: "back"}, "alice")
	if err != nil || inc.RecoveredAt != 0 || inc.Resolved != 0 {
		t.Fatalf("%v %+v", err, inc)
	}
	h.now = h.now.Add(25 * time.Hour)
	if h.m.tickAutoResolve() {
		t.Fatal("reopened incident re-resolved")
	}
	// same via editUpdate on the latest update
	if _, err := h.m.post(id, core.NewUpdate{Status: core.IncidentResolved, Message: "again"}, "alice"); err != nil {
		t.Fatal(err)
	}
	inc = h.m.data.Incidents[0]
	inc.RecoveredAt = h.now.Unix() // simulate a recovery recorded before the resolve
	h.m.data.Incidents[0] = inc
	last := inc.Updates[len(inc.Updates)-1].ID
	if _, err := h.m.editUpdate(id, last, core.NewUpdate{Status: core.IncidentMonitoring, Message: "again"}, "bob"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(25 * time.Hour)
	if h.m.tickAutoResolve() {
		t.Fatal("edit-reopened incident re-resolved")
	}
}

func TestIncidentEditUpdateStatusRules(t *testing.T) {
	h := newIMHarness(t, "api")
	inc, _ := h.m.create(core.NewIncident{Title: "T", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "a"}}, "alice")
	inc, _ = h.m.post(inc.ID, core.NewUpdate{Status: core.IncidentIdentified, Message: "b"}, "alice")
	first, last := inc.Updates[0].ID, inc.Updates[1].ID
	inc, err := h.m.editUpdate(inc.ID, first, core.NewUpdate{Status: core.IncidentResolved, Message: "a2"}, "bob")
	if err != nil || inc.Status != core.IncidentIdentified || inc.Resolved != 0 {
		t.Fatalf("older edit changed incident: %v %+v", err, inc)
	}
	inc, _ = h.m.editUpdate(inc.ID, last, core.NewUpdate{Status: core.IncidentResolved, Message: "b"}, "bob")
	if inc.Status != core.IncidentResolved || inc.Resolved == 0 {
		t.Fatalf("%+v", inc)
	}
	inc, _ = h.m.editUpdate(inc.ID, last, core.NewUpdate{Status: core.IncidentIdentified, Message: "b"}, "bob")
	if inc.Status != core.IncidentIdentified || inc.Resolved != 0 {
		t.Fatalf("%+v", inc)
	}
	if _, err := h.m.editUpdate(inc.ID, "nope", core.NewUpdate{Status: core.IncidentIdentified, Message: "b"}, "bob"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestIncidentEditIncident(t *testing.T) {
	h := newIMHarness(t, "api", "web")
	inc, _ := h.m.create(core.NewIncident{Title: "T", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "a"}}, "alice")
	inc, err := h.m.editIncident(inc.ID, "  New title ", []string{"api", "web"}, "bob")
	if err != nil || inc.Title != "New title" || len(inc.Services) != 2 {
		t.Fatalf("%v %+v", err, inc)
	}
	if _, err := h.m.editIncident(inc.ID, "", nil, "bob"); err == nil {
		t.Fatal("bad title accepted")
	}
	if _, err := h.m.editIncident(inc.ID, "ok", []string{"nope"}, "bob"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown service: %v", err)
	}
	if got := h.m.data.Incidents[0].Title; got != "New title" {
		t.Fatalf("rejected edit mutated: %q", got)
	}
}

func TestIncidentEchoOncePerNewUpdate(t *testing.T) {
	h := newIMHarness(t, "api")
	openAuto(h, "api")
	if len(h.echoes) != 1 {
		t.Fatalf("auto open: %v", h.echoes)
	}
	inc := h.m.data.Incidents[0]
	h.m.post(inc.ID, core.NewUpdate{Status: core.IncidentIdentified, Message: "x"}, "alice")
	if len(h.echoes) != 2 {
		t.Fatalf("post: %v", h.echoes)
	}
	h.m.editUpdate(inc.ID, inc.Updates[0].ID, core.NewUpdate{Status: core.IncidentInvestigating, Message: "y"}, "bob")
	h.m.editIncident(inc.ID, "Renamed", nil, "bob")
	if len(h.echoes) != 2 {
		t.Fatalf("edits echoed: %v", h.echoes)
	}
	h.m.create(core.NewIncident{Title: "M", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "m"}}, "alice")
	if len(h.echoes) != 3 {
		t.Fatalf("create: %v", h.echoes)
	}
}

func TestIncidentPersonResolvedRecoveredNotTouchedByTick(t *testing.T) {
	h := newIMHarness(t, "api")
	openAuto(h, "api")
	recover1(h, "api")
	id := h.m.data.Incidents[0].ID
	inc, _ := h.m.post(id, core.NewUpdate{Status: core.IncidentResolved, Message: "done"}, "alice")
	n, resolved := len(inc.Updates), inc.Resolved
	h.now = h.now.Add(48 * time.Hour)
	if h.m.tickAutoResolve() {
		t.Fatal("tick touched a resolved incident")
	}
	// posting resolved again keeps the original Resolved time
	inc, _ = h.m.post(id, core.NewUpdate{Status: core.IncidentResolved, Message: "again"}, "alice")
	if inc.Resolved != resolved || len(inc.Updates) != n+1 {
		t.Fatalf("resolved %d want %d", inc.Resolved, resolved)
	}
}

func TestIncidentTriggersCapped(t *testing.T) {
	h := newIMHarness(t, "api")
	openAuto(h, "api")
	for i := 0; i < 25; i++ {
		recover1(h, "api")
		h.setState("api", core.StateOutage)
		h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage, Reason: fmt.Sprint("r", i)}})
	}
	inc := h.m.data.Incidents[0]
	if len(h.m.data.Incidents) != 1 || len(inc.Triggers) != 20 || inc.Triggers[19] != "api: r24" {
		t.Fatalf("n=%d triggers %v", len(h.m.data.Incidents), inc.Triggers)
	}
}

func TestIncidentReturnedCopiesAreDeep(t *testing.T) {
	h := newIMHarness(t, "api")
	inc, _ := h.m.create(core.NewIncident{Title: "T", Services: []string{"api"}, Impact: core.StateDegraded,
		Update: core.NewUpdate{Status: core.IncidentInvestigating, Message: "a"}}, "alice")
	inc, _ = h.m.editUpdate(inc.ID, inc.Updates[0].ID, core.NewUpdate{Status: core.IncidentInvestigating, Message: "b"}, "bob")
	inc.Updates[0].Message = "mutated"
	inc.Updates[0].Edits[0].Message = "mutated"
	inc.Services[0] = "mutated"
	got := h.m.data.Incidents[0]
	if got.Updates[0].Message != "b" || got.Updates[0].Edits[0].Message != "a" || got.Services[0] != "api" {
		t.Fatalf("stored incident aliased: %+v", got)
	}
}

func TestSweepRecoversIncidentOfDeletedService(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	if h.m.sweepRecovered() {
		t.Fatal("swept while the service is still in outage")
	}
	h.m.data.Services = nil
	delete(h.m.data.State, "api")
	if !h.m.sweepRecovered() {
		t.Fatal("deleted service: no sweep")
	}
	inc := h.m.data.Incidents[0]
	if inc.Status != core.IncidentMonitoring || inc.RecoveredAt == 0 {
		t.Fatalf("not recovered: %+v", inc)
	}
	if h.m.sweepRecovered() {
		t.Fatal("swept twice")
	}
}

func TestSweepRecoversIncidentOfEditedOutService(t *testing.T) {
	h := newIMHarness(t, "api", "web")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.setState("api", core.StateOperational) // edited so it no longer fails; no change event reached the manager
	if !h.m.sweepRecovered() {
		t.Fatal("edited-out service: no sweep")
	}
	if inc := h.m.data.Incidents[0]; inc.Status != core.IncidentMonitoring || inc.RecoveredAt == 0 {
		t.Fatalf("not recovered: %+v", inc)
	}
	h.now = h.now.Add(25 * time.Hour)
	if !h.m.tickAutoResolve() || h.m.data.Incidents[0].Status != core.IncidentResolved {
		t.Fatal("swept incident did not auto-resolve")
	}
}

func TestSweepLeavesHumanReopenedIncidentAlone(t *testing.T) {
	h := newIMHarness(t, "api")
	h.setState("api", core.StateOutage)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOperational, To: core.StateOutage}})
	h.setState("api", core.StateOperational)
	h.m.applyChanges([]stateChange{{ServiceID: "api", From: core.StateOutage, To: core.StateOperational}})
	h.now = h.now.Add(25 * time.Hour)
	h.m.tickAutoResolve()
	id := h.m.data.Incidents[0].ID
	if h.m.data.Incidents[0].Status != core.IncidentResolved {
		t.Fatal("setup: not resolved")
	}
	if _, err := h.m.post(id, core.NewUpdate{Status: core.IncidentInvestigating, Message: "still odd"}, "alice"); err != nil {
		t.Fatal(err)
	}
	n := len(h.m.data.Incidents[0].Updates)
	for i := 0; i < 2; i++ { // per-tick entry points, then again after > auto_resolve_after
		h.m.applyChanges(nil)
		h.m.sweepRecovered()
		h.m.tickAutoResolve()
		h.now = h.now.Add(25 * time.Hour)
	}
	inc := h.m.data.Incidents[0]
	if inc.Status != core.IncidentInvestigating || len(inc.Updates) != n || inc.RecoveredAt != 0 {
		t.Fatalf("human reopen overridden: %+v", inc)
	}
}
