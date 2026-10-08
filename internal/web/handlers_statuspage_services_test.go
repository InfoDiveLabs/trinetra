package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func statusPageDeps(t *testing.T, sp *fakeStatusPage) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.StatusPage = func() core.StatusPageAPI { return sp }
	return d
}

func getAsRole(t *testing.T, d Deps, role Role, path string) (int, string) {
	t.Helper()
	rr := fleetGetAsRole(t, d, role, path)
	return rr.Code, rr.Body.String()
}

func postAsRole(t *testing.T, d Deps, role Role, path string, form url.Values) int {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	u := &User{ID: mustNewUserID(t), Name: string(role) + "-user", Role: role, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sess.ID}
	rr := postForm(h, path, form, cookie, sess.CSRF)
	return rr.Code
}

func TestStatusServicesPageRBAC(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API", Targets: []core.StatusTarget{{Kind: core.TargetHost}}}},
		evals: []core.ServiceEvaluation{{ServiceID: "api", State: core.StateDegraded, Reason: "warning alert cpu on this host"}}}
	d := statusPageDeps(t, sp)
	for role, want := range map[Role]int{RoleViewer: 403, RoleResponder: 403, RoleAdmin: 200} {
		code, body := getAsRole(t, d, role, "/status-page/services")
		if code != want {
			t.Errorf("%s: %d want %d", role, code, want)
		}
		if role == RoleAdmin && (!strings.Contains(body, "API") || !strings.Contains(body, "warning alert cpu") || !strings.Contains(body, "pill-degraded")) {
			t.Errorf("admin page missing service/reason: %s", body)
		}
	}
}

func TestStatusServiceSaveValidatesAndAudits(t *testing.T) {
	sp := &fakeStatusPage{}
	d := statusPageDeps(t, sp)
	form := url.Values{"id": {"api"}, "name": {"API"}, "hold": {"180"}, "targets": {"tag:api\ncontainer:pg@n1"}}
	code := postAsRole(t, d, RoleAdmin, "/status-page/services", form)
	if code != http.StatusSeeOther {
		t.Fatalf("save: %d", code)
	}
	if len(sp.svcs) != 1 || len(sp.svcs[0].Targets) != 2 || sp.svcs[0].HoldDownSec != 180 {
		t.Fatalf("%+v", sp.svcs)
	}
	if !strings.Contains(readAuditFileRaw(t, d.StateDir), "status_page.service.save") {
		t.Fatal("not audited")
	}
	bad := url.Values{"id": {"API!"}, "name": {"x"}, "hold": {"180"}, "targets": {"host"}}
	if code := postAsRole(t, d, RoleAdmin, "/status-page/services", bad); code != http.StatusBadRequest {
		t.Fatalf("invalid id: %d", code)
	}
	if code := postAsRole(t, d, RoleResponder, "/status-page/services", form); code != http.StatusForbidden {
		t.Fatalf("responder save: %d", code)
	}
}

func TestStatusServiceDeleteAudits(t *testing.T) {
	sp := &fakeStatusPage{}
	d := statusPageDeps(t, sp)
	if code := postAsRole(t, d, RoleAdmin, "/status-page/services/api/delete", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if len(sp.actions) != 1 || !strings.HasPrefix(sp.actions[0], "delsvc:api:") {
		t.Fatalf("%v", sp.actions)
	}
	if !strings.Contains(readAuditFileRaw(t, d.StateDir), "status_page.service.delete") {
		t.Fatal("not audited")
	}
	if code := postAsRole(t, d, RoleViewer, "/status-page/services/api/delete", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("viewer delete: %d", code)
	}
}

func TestStatusServicesOnChildShowsNotice(t *testing.T) {
	sp := &fakeStatusPage{err: core.ErrStatusPageOnChild}
	code, body := getAsRole(t, statusPageDeps(t, sp), RoleAdmin, "/status-page/services")
	if code != 200 || !strings.Contains(body, "runs on the fleet master") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestStatusServicesUnwiredIsUnavailable(t *testing.T) {
	code, body := getAsRole(t, enrollTestDeps(t), RoleAdmin, "/status-page/services")
	if code != 200 || !strings.Contains(body, "not available") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestStatusServiceStickyFormOn400(t *testing.T) {
	d := statusPageDeps(t, &fakeStatusPage{})
	form := url.Values{"id": {"API!"}, "name": {"My Name"}, "group": {"G1"}, "hold": {"180"}, "description": {"desc here"}, "targets": {"tag:keepme\nhost"}}
	rr := postFormAsRole(t, d, RoleAdmin, "/status-page/services", form)
	body := rr.Body.String()
	if rr.Code != 400 {
		t.Fatalf("%d", rr.Code)
	}
	for _, want := range []string{"My Name", "tag:keepme", "G1", "desc here", "API!"} {
		if !strings.Contains(body, want) {
			t.Errorf("sticky form missing %q", want)
		}
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Error("no error message")
	}
}

func TestStatusServiceEditPrefill(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "Public API", HoldDownSec: 90,
		Targets: []core.StatusTarget{{Kind: core.TargetTag, Value: "web"}, {Kind: core.TargetContainer, Value: "pg", Node: "n1"}}}}}
	d := statusPageDeps(t, sp)
	_, body := getAsRole(t, d, RoleAdmin, "/status-page/services?edit=api")
	for _, want := range []string{"Edit service Public API", "readonly", "tag:web\ncontainer:pg@n1", `value="90"`, "/status-page/services?edit=api"} {
		if !strings.Contains(body, want) {
			t.Errorf("edit page missing %q", want)
		}
	}
	_, body = getAsRole(t, d, RoleAdmin, "/status-page/services?edit=nope")
	if strings.Contains(body, "Edit service") || strings.Contains(body, "readonly") {
		t.Error("unknown edit id should show the empty add form")
	}
}

func TestStatusServiceSaveRejections(t *testing.T) {
	d := statusPageDeps(t, &fakeStatusPage{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)
	good := url.Values{"id": {"api"}, "name": {"API"}, "hold": {"60"}, "targets": {"host"}}
	if rr := postForm(h, "/status-page/services", good, cookie, ""); rr.Code != http.StatusForbidden {
		t.Errorf("no CSRF: %d", rr.Code)
	}
	badKind := url.Values{"id": {"api"}, "name": {"API"}, "hold": {"60"}, "targets": {"foo:bar"}}
	if c := postAsRole(t, d, RoleAdmin, "/status-page/services", badKind); c != 400 {
		t.Errorf("unknown kind: %d", c)
	}
	badOrder := url.Values{"id": {"api"}, "name": {"API"}, "hold": {"60"}, "order": {"x"}, "targets": {"host"}}
	if c := postAsRole(t, d, RoleAdmin, "/status-page/services", badOrder); c != 400 {
		t.Errorf("bad order: %d", c)
	}
}

func TestStatusServicesEvaluationErrorShowsNotice(t *testing.T) {
	sp := &evalErrStatusPage{fakeStatusPage: &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "API"}}}}
	d := enrollTestDeps(t)
	d.StatusPage = func() core.StatusPageAPI { return sp }
	_, body := getAsRole(t, d, RoleAdmin, "/status-page/services")
	if !strings.Contains(body, "Live status unavailable") {
		t.Error("no notice")
	}
}

type evalErrStatusPage struct{ *fakeStatusPage }

func (e *evalErrStatusPage) Evaluation() ([]core.ServiceEvaluation, error) {
	return nil, errors.New("boom")
}

func TestStatusServicesNodeScoped404(t *testing.T) {
	d := statusPageDeps(t, &fakeStatusPage{})
	if code, _ := getAsRole(t, d, RoleAdmin, "/n/x/status-page/services"); code != 404 {
		t.Errorf("node-scoped: %d", code)
	}
}

func postFormAsRole(t *testing.T, d Deps, role Role, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	u := &User{ID: mustNewUserID(t), Name: string(role) + "-user", Role: role, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatal(err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return postForm(h, path, form, &http.Cookie{Name: sessionCookieName, Value: sess.ID}, sess.CSRF)
}

func TestStatusServiceAddRejectsExistingIDEditRequiresIt(t *testing.T) {
	sp := &fakeStatusPage{svcs: []core.StatusService{{ID: "api", Name: "Orig", HoldDownSec: 180, Targets: []core.StatusTarget{{Kind: core.TargetHost}}}}}
	d := statusPageDeps(t, sp)
	form := url.Values{"id": {"api"}, "name": {"Clobber"}, "hold": {"180"}, "targets": {"host"}}
	rr := postFormAsRole(t, d, RoleAdmin, "/status-page/services", form)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "already exists") {
		t.Fatalf("add existing: %d %s", rr.Code, rr.Body.String())
	}
	if sp.svcs[0].Name != "Orig" {
		t.Fatalf("overwritten: %+v", sp.svcs)
	}
	form.Set("edit", "1")
	if code := postAsRole(t, d, RoleAdmin, "/status-page/services", form); code != http.StatusSeeOther {
		t.Fatalf("edit existing: %d", code)
	}
	if sp.svcs[0].Name != "Clobber" {
		t.Fatalf("edit not applied: %+v", sp.svcs)
	}
	form.Set("id", "ghost")
	rr = postFormAsRole(t, d, RoleAdmin, "/status-page/services", form)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "no such service") {
		t.Fatalf("edit missing: %d %s", rr.Code, rr.Body.String())
	}
}
