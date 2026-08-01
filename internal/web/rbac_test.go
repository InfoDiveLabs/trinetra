package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// rbacTestDeps builds Deps + the UserStore/SessionStore instances pointed at
// the same fresh StateDir newHandler(d) will use, mirroring enrollTestDeps.
func rbacTestDeps(t *testing.T) (Deps, UserStore, SessionStore) {
	t.Helper()
	d := enrollTestDeps(t)
	return d, newUserStore(d.StateDir), newSessionStore(d.StateDir)
}

// seedSignedInRequest creates a user with the given role plus a live
// session for it, and returns a request carrying the sw_session cookie.
// RBAC only cares what a resolved session/user look like, which sessions.New
// + store.Put produce directly — no need for a full WebAuthn ceremony here
// (that's what auth_webauthn_test.go/auth_login_test.go's virtual
// authenticator pins instead).
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

// TestRequireRoleAnonRedirectsToLogin pins the anonymous case: no session at
// all on an admin route must redirect to /login rather than 403 (there's no
// "your role is wrong" to report — there's no session to have a role).
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

// TestRequireRoleViewerBlockedFromAdminRoute pins the RBAC matrix's core
// negative case: a signed-in viewer hitting an admin route gets 403 and the
// mockup's "Admin only" denied panel, not a redirect (they ARE
// authenticated, just not authorized).
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
	if !strings.Contains(body, "Admin only") {
		t.Errorf("403 body missing \"Admin only\" heading:\n%s", body)
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

// TestRequireRoleAppliesAcrossAllAdminRoutes pins that every admin nav
// target the mockup's ADMIN_PAGES list names (config/channels/users/
// public-settings) is actually gated, not just /config.
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
