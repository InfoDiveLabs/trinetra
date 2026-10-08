package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func incidentFixture() *fakeStatusPage {
	return &fakeStatusPage{
		svcs: []core.StatusService{{ID: "api", Name: "API"}},
		incs: []core.StatusIncident{{ID: "i1", Title: "Outage: API", Impact: core.StateOutage, Services: []string{"api"},
			Status: core.IncidentInvestigating, Auto: true, Triggers: []string{"api: db-01 is down"},
			Updates: []core.IncidentUpdate{{ID: "u1", Status: core.IncidentInvestigating, Message: "We're investigating an outage affecting API.", Author: "system"}}}},
	}
}

func TestIncidentPagesRBAC(t *testing.T) {
	d := statusPageDeps(t, incidentFixture())
	for _, path := range []string{"/status-page/incidents", "/status-page/incidents/i1"} {
		for role, want := range map[Role]int{RoleViewer: 403, RoleResponder: 200, RoleAdmin: 200} {
			if code, _ := getAsRole(t, d, role, path); code != want {
				t.Errorf("%s as %s: %d want %d", path, role, code, want)
			}
		}
	}
}

func TestIncidentDetailShowsTriggersToResponders(t *testing.T) {
	_, body := getAsRole(t, statusPageDeps(t, incidentFixture()), RoleResponder, "/status-page/incidents/i1")
	if !strings.Contains(body, "db-01 is down") || !strings.Contains(body, "We&#39;re investigating") {
		t.Fatalf("detail body: %s", body)
	}
}

func TestIncidentDetailEscapesHostileMessage(t *testing.T) {
	sp := incidentFixture()
	sp.incs[0].Updates[0].Message = "<script>alert(1)</script>\nsecond line"
	_, body := getAsRole(t, statusPageDeps(t, sp), RoleResponder, "/status-page/incidents/i1")
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("script rendered unescaped")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;<br>second line") {
		t.Fatalf("expected escaped text with <br>: %s", body)
	}
}

func TestResponderPostsUpdate(t *testing.T) {
	sp := incidentFixture()
	d := statusPageDeps(t, sp)
	form := url.Values{"status": {"identified"}, "message": {"Root cause found <b>db</b>"}}
	if code := postAsRole(t, d, RoleResponder, "/status-page/incidents/i1/updates", form); code != http.StatusSeeOther {
		t.Fatalf("post: %d", code)
	}
	if got := sp.incs[0].Updates[len(sp.incs[0].Updates)-1]; got.Message != "Root cause found <b>db</b>" || got.Status != "identified" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(readAuditFileRaw(t, d.StateDir), "status_page.update.post") {
		t.Fatal("not audited")
	}
	_, body := getAsRole(t, d, RoleResponder, "/status-page/incidents/i1")
	if strings.Contains(body, "<b>db</b>") {
		t.Fatal("message rendered unescaped")
	}
	if code := postAsRole(t, d, RoleViewer, "/status-page/incidents/i1/updates", form); code != http.StatusForbidden {
		t.Fatalf("viewer post: %d", code)
	}
	if code := postAsRole(t, d, RoleResponder, "/status-page/incidents/i1/updates", url.Values{"status": {"identified"}, "message": {""}}); code != http.StatusBadRequest {
		t.Fatalf("empty message: %d", code)
	}
}

func TestResponderCreatesIncidentAndOnlyAdminDeletes(t *testing.T) {
	sp := incidentFixture()
	d := statusPageDeps(t, sp)
	form := url.Values{"title": {"Payments delayed"}, "services": {"api"}, "impact": {"degraded"}, "status": {"investigating"}, "message": {"Looking"}}
	if code := postAsRole(t, d, RoleResponder, "/status-page/incidents", form); code != http.StatusSeeOther {
		t.Fatalf("create: %d", code)
	}
	if code := postAsRole(t, d, RoleResponder, "/status-page/incidents/i1/delete", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("responder delete: %d", code)
	}
	if code := postAsRole(t, d, RoleAdmin, "/status-page/incidents/i1/delete", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("admin delete: %d", code)
	}
}

func TestIncidentUnknownIs404(t *testing.T) {
	if code, _ := getAsRole(t, statusPageDeps(t, incidentFixture()), RoleResponder, "/status-page/incidents/nope"); code != 404 {
		t.Fatalf("%d", code)
	}
}

func TestResponderCreateUnknownServiceIs400(t *testing.T) {
	sp := incidentFixture()
	sp.err = core.ErrNotFound
	d := statusPageDeps(t, sp)
	form := url.Values{"title": {"X"}, "services": {"ghost"}, "impact": {"degraded"}, "status": {"investigating"}, "message": {"m"}}
	if code := postAsRole(t, d, RoleResponder, "/status-page/incidents", form); code != http.StatusBadRequest {
		t.Fatalf("create with unknown service: %d want 400", code)
	}
}
