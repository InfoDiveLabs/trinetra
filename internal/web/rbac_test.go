package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// rbacTestDeps builds Deps + the UserStore/SessionStore instances pointed at
// the same fresh StateDir newHandler(d) will use, mirroring enrollTestDeps.
func rbacTestDeps(t *testing.T) (Deps, UserStore, SessionStore) {
	t.Helper()
	d := enrollTestDeps(t)
	return d, newUserStore(d.StateDir), newSessionStore(d.StateDir)
}

// seedSignedInRequest creates a user with the given role plus a live session for it, and
// returns a request carrying the sw_session cookie.
func seedSignedInRequest(t *testing.T, users UserStore, sessions SessionStore, role Role, method, target string) *http.Request {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: string(role) + "-user", Role: role, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed user Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	req := httptest.NewRequest(method, target, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	return req
}

// TestRequireRoleAnonRedirectsToLogin pins the anonymous case: no session at all on an
// admin route must redirect to /login rather than 403.
func TestRequireRoleAnonRedirectsToLogin(t *testing.T) {
	d, _, _ := rbacTestDeps(t)
	h := newHandler(d)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/config", nil))

	if rr.Code != http.StatusFound {
		t.Fatalf("GET /config anon status = %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

// TestRequireRoleViewerBlockedFromAdminRoute pins the RBAC matrix's core negative case: a
// signed-in viewer hitting an admin route gets 403 and the "Admin only" denied panel.
func TestRequireRoleViewerBlockedFromAdminRoute(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/config")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("GET /config as viewer status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Higher role needed") {
		t.Errorf("403 body missing \"Higher role needed\" heading:\n%s", body)
	}
	if !strings.Contains(body, `class="panel denied"`) {
		t.Errorf("403 body missing the mockup's denied-panel markup:\n%s", body)
	}
}

// TestRequireRoleAdminAllowed pins the positive case: an admin session
// reaches the admin route's actual handler (200).
func TestRequireRoleAdminAllowed(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config as admin status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
}

// TestRequireRoleAppliesAcrossAllAdminRoutes pins that every admin nav target
// (config/channels/users/public-settings) is actually gated, not just /config.
func TestRequireRoleAppliesAcrossAllAdminRoutes(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	for _, route := range []string{"/config", "/channels", "/users", "/settings/public"} {
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, route)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("GET %s as viewer status = %d, want 403", route, rr.Code)
		}
	}
}

// TestRequireRoleAnonRedirectsAcrossAllAdminRoutes is TestRequireRoleAnonRedirectsToLogin
// generalized the same way.
func TestRequireRoleAnonRedirectsAcrossAllAdminRoutes(t *testing.T) {
	d, _, _ := rbacTestDeps(t)
	h := newHandler(d)
	for _, route := range []string{"/config", "/channels", "/users", "/settings/public"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route, nil))
		if rr.Code != http.StatusFound {
			t.Errorf("GET %s anon status = %d, want %d", route, rr.Code, http.StatusFound)
		}
	}
}

func TestRequireRoleRanks(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	d, _, _ := rbacTestDeps(t)
	cases := []struct {
		min, have Role
		want      int
	}{
		{RoleViewer, RoleViewer, 204}, {RoleViewer, RoleResponder, 204}, {RoleViewer, RoleAdmin, 204},
		{RoleResponder, RoleViewer, 403}, {RoleResponder, RoleResponder, 204}, {RoleResponder, RoleAdmin, 204},
		{RoleAdmin, RoleResponder, 403}, {RoleAdmin, RoleAdmin, 204},
	}
	for _, c := range cases {
		// requireRole reads the user userMiddleware would have resolved.
		r := httptest.NewRequest("GET", "/x", nil)
		r = r.WithContext(context.WithValue(r.Context(), userCtxKey{}, &User{ID: "u", Name: "u", Role: c.have}))
		w := httptest.NewRecorder()
		requireRole(c.min, d, ok).ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("min=%s have=%s: %d want %d", c.min, c.have, w.Code, c.want)
		}
	}
}

func TestRoleRankAndValid(t *testing.T) {
	if !(roleRank(RoleViewer) < roleRank(RoleResponder) && roleRank(RoleResponder) < roleRank(RoleAdmin) && roleRank(RoleViewer) > 0) {
		t.Error("rank order must be viewer < responder < admin, all > 0")
	}
	for _, r := range []Role{"", "superuser"} {
		if validRole(r) {
			t.Errorf("validRole(%q) = true", r)
		}
	}
}

// A responder may ack/unack alerts but must be denied every admin page and mutation.
func TestResponderCanAckAlerts(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	mk := func(role Role) (*http.Cookie, string) {
		u := &User{ID: mustNewUserID(t), Name: string(role), Role: role, Created: 1}
		if err := users.Put(u); err != nil {
			t.Fatal(err)
		}
		s, err := sessions.New(u.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Cookie{Name: sessionCookieName, Value: s.ID}, s.CSRF
	}
	ack := "/alerts/" + credentialParam([]byte("disk:/")) + "/ack"

	vc, vcsrf := mk(RoleViewer)
	if rr := postForm(h, ack, nil, vc, vcsrf); rr.Code != http.StatusForbidden {
		t.Errorf("viewer ack = %d, want 403", rr.Code)
	}
	rc, rcsrf := mk(RoleResponder)
	if rr := postForm(h, ack, nil, rc, rcsrf); rr.Code == http.StatusForbidden {
		t.Errorf("responder ack = 403, want handler reached: %s", rr.Body.String())
	}
	// Alerts page shows the ack button to a responder.
	req := httptest.NewRequest(http.MethodGet, "/alerts", nil)
	req.AddCookie(rc)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), ">Ack</button>") {
		t.Errorf("responder /alerts = %d, ack button present=%v", rr.Code, strings.Contains(rr.Body.String(), ">Ack</button>"))
	}
}

func TestResponderDeniedAdminRoutes(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	u := &User{ID: mustNewUserID(t), Name: "resp", Role: RoleResponder, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatal(err)
	}
	s, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: s.ID}
	for _, p := range []string{"/users", "/config", "/channels", "/settings/public"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.AddCookie(cookie)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("responder GET %s = %d, want 403", p, rr.Code)
		}
	}
	rr := postForm(h, "/users/invite", url.Values{"role": {"admin"}, "ttl": {"1h"}}, cookie, s.CSRF)
	if rr.Code != http.StatusForbidden {
		t.Errorf("responder POST /users/invite = %d, want 403", rr.Code)
	}
}

func TestResponderCanUnackAlerts(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	u := &User{ID: mustNewUserID(t), Name: "resp", Role: RoleResponder, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatal(err)
	}
	s, _ := sessions.New(u.ID, time.Hour)
	rr := postForm(h, "/alerts/"+credentialParam([]byte("disk:/"))+"/unack", nil, &http.Cookie{Name: sessionCookieName, Value: s.ID}, s.CSRF)
	if rr.Code == http.StatusForbidden {
		t.Errorf("responder unack = 403: %s", rr.Body.String())
	}
}

func TestResolveEnrollRoleRejectsInvalidTokenRole(t *testing.T) {
	d, users, _ := rbacTestDeps(t)
	tokens := newTokenStore(d.StateDir)
	tok := tokens.Issue(Role("superuser"), time.Hour)
	if _, _, err := resolveEnrollRole(tokens, users, tok); err == nil {
		t.Error("invalid token role accepted")
	}
	tok = tokens.Issue(RoleResponder, time.Hour)
	if r, _, err := resolveEnrollRole(tokens, users, tok); err != nil || r != RoleResponder {
		t.Errorf("responder token = %q, %v", r, err)
	}
}
