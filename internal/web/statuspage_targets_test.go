package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

var pickMonitoring = core.MonitoringView{
	Containers:   []core.MonitoringContainerView{{Name: "web", State: "running"}, {Name: "postgres", State: "running"}},
	UnitsEnabled: true,
	Units:        []core.MonitoringUnitView{{Name: "nginx.service", Active: "active"}},
}

func pickDeps(t *testing.T, sp *fakeStatusPage) Deps {
	t.Helper()
	d := statusPageDeps(t, sp)
	d.API = fakeAPI{monitoring: pickMonitoring}
	return d
}

func TestMonitoringOffersStatusPagePickToAdminsOnly(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "site", Name: "Website", Group: "Public", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}}}
	d := pickDeps(t, sp)
	_, body := getAsRole(t, d, RoleAdmin, "/monitoring")
	for _, want := range []string{`value="container:web"`, `value="unit:nginx.service"`, `form="sp-add"`,
		`action="/status-page/services/add-targets"`, `<option value="site">Website · Public</option>`, `<option value="Public">`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin monitoring missing %s", want)
		}
	}
	_, body = getAsRole(t, d, RoleResponder, "/monitoring")
	if strings.Contains(body, "sp-add") {
		t.Error("responder sees the status page picker")
	}
	sp.err = core.ErrStatusPageOnChild
	_, body = getAsRole(t, d, RoleAdmin, "/monitoring")
	if strings.Contains(body, "sp-add") {
		t.Error("picker shown where the status page is unavailable")
	}
}

func TestMonitoringPickOnNodeScopesTargets(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}, nodes: []core.NodeSummary{
		{ID: core.SelfNodeID, Self: true, State: "online"}, {ID: "child1", Name: "db-01", State: "online"}}}
	d := fleetTestDeps(t, masterFakeAPI(fleet, map[string]core.API{"child1": fakeAPI{monitoring: pickMonitoring}}))
	d.StatusPage = func() core.StatusPageAPI { return &fakeStatusPage{} }
	_, body := getAsRole(t, d, RoleAdmin, "/n/child1/monitoring")
	if !strings.Contains(body, `value="container:postgres@child1"`) || !strings.Contains(body, `value="unit:nginx.service@child1"`) {
		t.Errorf("node-scoped picker values missing @child1:\n%s", body)
	}
}

func TestAddTargetsCreatesNewService(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "website", Name: "Old", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}}}
	d := pickDeps(t, sp)
	form := url.Values{"mode": {"new"}, "name": {"Website"}, "group": {"Public"},
		"target": {"container:web", "unit:nginx.service@child1", "container:web"}}
	rr := postFormAsRole(t, d, RoleAdmin, "/status-page/services/add-targets", form)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/status-page/services?added=2&to=website-2" {
		t.Fatalf("got %d %q %s", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	got := sp.svcs[1]
	if got.ID != "website-2" || got.Name != "Website" || got.Group != "Public" || got.HoldDownSec != core.DefaultHoldDownSec || len(got.Targets) != 2 {
		t.Fatalf("new service: %+v", got)
	}
	if got.Targets[1] != (core.StatusTarget{Kind: core.TargetUnit, Value: "nginx.service", Node: "child1"}) {
		t.Errorf("target parsed wrong: %+v", got.Targets[1])
	}
}

func TestAddTargetsMergesIntoExisting(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API", HoldDownSec: 60,
		Targets: []core.StatusTarget{{Kind: core.TargetContainer, Value: "web"}}}}}
	d := pickDeps(t, sp)
	form := url.Values{"mode": {"existing"}, "service": {"api"}, "target": {"container:web", "container:postgres"}}
	rr := postFormAsRole(t, d, RoleAdmin, "/status-page/services/add-targets", form)
	if rr.Code != http.StatusSeeOther || !strings.Contains(rr.Header().Get("Location"), "added=1") {
		t.Fatalf("got %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if s := sp.svcs[0]; len(s.Targets) != 2 || s.HoldDownSec != 60 || s.Name != "API" {
		t.Fatalf("merge: %+v", s)
	}
	_, body := getAsRole(t, d, RoleAdmin, "/status-page/services?added=1&to=api")
	if !strings.Contains(body, "Added 1 item to API.") {
		t.Error("no confirmation after adding")
	}
}

func TestAddTargetsRejections(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}}}
	d := pickDeps(t, sp)
	for name, form := range map[string]url.Values{
		"nothing selected": {"mode": {"new"}, "name": {"X"}},
		"no name":          {"mode": {"new"}, "target": {"container:web"}},
		"unknown service":  {"mode": {"existing"}, "service": {"ghost"}, "target": {"container:web"}},
		"no mode":          {"target": {"container:web"}},
	} {
		if rr := postFormAsRole(t, d, RoleAdmin, "/status-page/services/add-targets", form); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	form := url.Values{"mode": {"new"}, "name": {"X"}, "target": {"container:web"}}
	if rr := postFormAsRole(t, d, RoleResponder, "/status-page/services/add-targets", form); rr.Code != http.StatusForbidden {
		t.Errorf("responder: %d", rr.Code)
	}
	if len(sp.svcs) != 1 {
		t.Fatalf("rejected requests changed services: %+v", sp.svcs)
	}
}

func TestServicesFormChecklist(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API", Group: "Core", Targets: []core.StatusTarget{
		{Kind: core.TargetContainer, Value: "web"}, {Kind: core.TargetTag, Value: "edge"}}}}}
	d := pickDeps(t, sp)
	_, body := getAsRole(t, d, RoleAdmin, "/status-page/services?edit=api")
	if !strings.Contains(body, `value="container:web" checked`) || strings.Contains(body, `value="container:postgres" checked`) {
		t.Error("checklist does not reflect the service's targets")
	}
	if !strings.Contains(body, ">tag:edge</textarea>") {
		t.Error("targets the checklist can't show should stay in the text field")
	}
	if !strings.Contains(body, `list="sp-groups"`) || !strings.Contains(body, `<option value="Core">`) {
		t.Error("group picker missing")
	}

	form := url.Values{"id": {"api"}, "name": {"API"}, "hold": {"180"}, "edit": {"1"},
		"target": {"container:postgres", "host"}, "targets": {"tag:edge"}}
	if code := postAsRole(t, d, RoleAdmin, "/status-page/services", form); code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	if got := sp.svcs[0].Targets; len(got) != 3 || got[0].Value != "postgres" || got[1].Kind != core.TargetHost || got[2].Value != "edge" {
		t.Fatalf("saved targets: %+v", got)
	}
	form.Del("target")
	form.Set("targets", "")
	if code := postAsRole(t, d, RoleAdmin, "/status-page/services", form); code != http.StatusBadRequest {
		t.Errorf("empty targets accepted: %d", code)
	}
}

func TestServiceSlug(t *testing.T) {
	taken := map[string]bool{"api": true, "api-2": true}
	for in, want := range map[string]string{"API": "api-3", "Payments & Billing": "payments-billing", "!!!": "service", " Web Site ": "web-site"} {
		if got := serviceSlug(in, taken); got != want {
			t.Errorf("serviceSlug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := serviceSlug(strings.Repeat("a", 60), nil); len(got) > 40 {
		t.Errorf("slug too long: %q", got)
	}
}
