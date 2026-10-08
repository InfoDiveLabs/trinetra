package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

func TestIncidentFailedPostKeepsDraft(t *testing.T) {
	d := statusPageDeps(t, incidentFixture())
	h := func(form url.Values) (int, string) {
		rr := postBodyAsRole(t, d, RoleResponder, "/status-page/incidents/i1/updates", form)
		return rr.Code, rr.Body.String()
	}
	code, body := h(url.Values{"status": {"monitoring"}, "message": {""}})
	if code != 400 || !strings.Contains(body, `<option selected>monitoring</option>`) {
		t.Fatalf("status not kept: %d %s", code, body)
	}
	long := "keep<me> " + strings.Repeat("x", core.MaxUpdateBytes)
	code, body = h(url.Values{"status": {"identified"}, "message": {long}})
	if code != 400 || !strings.Contains(body, "keep&lt;me&gt; xxxx") {
		t.Fatalf("long message not kept: %d", code)
	}
}

func TestIncidentFailedCreateKeepsForm(t *testing.T) {
	d := statusPageDeps(t, incidentFixture())
	rr := postBodyAsRole(t, d, RoleResponder, "/status-page/incidents", url.Values{"title": {"My title"}, "services": {"api"},
		"impact": {"outage"}, "status": {"monitoring"}, "message": {"   "}})
	body := rr.Body.String()
	if rr.Code != 400 {
		t.Fatalf("%d", rr.Code)
	}
	for _, want := range []string{`value="My title"`, `value="api" checked`, `<option selected>outage</option>`, `<option selected>monitoring</option>`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
}

func TestIncidentDetailOnChildShowsNoticeNoForms(t *testing.T) {
	sp := incidentFixture()
	sp.err = core.ErrStatusPageOnChild
	_, body := getAsRole(t, statusPageDeps(t, sp), RoleResponder, "/status-page/incidents/i1")
	if !strings.Contains(body, "runs on the fleet master") {
		t.Fatalf("no notice: %s", body)
	}
	if strings.Contains(body, "action=\"/status-page/incidents//") {
		t.Fatal("dead forms rendered")
	}
}

// postBodyAsRole is postAsRole but returns the whole recorder.
func postBodyAsRole(t *testing.T, d Deps, role Role, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: string(role) + "-user", Role: role, Created: 1}
	if err := newUserStore(d.StateDir).Put(u); err != nil {
		t.Fatal(err)
	}
	sess, err := newSessionStore(d.StateDir).New(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return postForm(newHandler(d), path, form, &http.Cookie{Name: sessionCookieName, Value: sess.ID}, sess.CSRF)
}

func TestIncidentPagesRenderCompletely(t *testing.T) {
	d := statusPageDeps(t, incidentFixture())
	for _, path := range []string{"/status-page/incidents", "/status-page/incidents/i1"} {
		_, body := getAsRole(t, d, RoleAdmin, path)
		if !strings.Contains(body, "</html>") {
			t.Errorf("%s truncated (template exec error)", path)
		}
	}
	if !strings.Contains(func() string { _, b := getAsRole(t, d, RoleAdmin, "/status-page/incidents"); return b }(), "Outage: API") {
		t.Error("list missing incident")
	}
}
