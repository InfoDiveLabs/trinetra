package web

import (
	"net/http"
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
