//go:build web

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seedAdmin puts a RoleAdmin *User in store and mints a live session for it,
// returning the session cookie plus that session's CSRF token — everything a
// test needs to act as that admin against a CSRF-protected /users/* mutation.
func seedAdmin(t *testing.T, name string, users UserStore, sessions SessionStore) (*User, *http.Cookie, string) {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: name, Role: RoleAdmin, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed admin Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	return u, &http.Cookie{Name: sessionCookieName, Value: sess.ID}, sess.CSRF
}

// postForm issues a form-encoded POST against h, optionally attaching a
// session cookie and/or an X-CSRF-Token header — the shape every /users/*
// mutation test needs (some deliberately omit the CSRF header to pin the
// requireCSRF gate).
func postForm(h http.Handler, target string, form url.Values, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestUsersPageListsUsers pins the admin roster view: names, role badges,
// created dates, and passkey counts (including the zero-passkey case) all
// show up for an admin viewing GET /users.
func TestUsersPageListsUsers(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)

	_, cookie, _ := seedAdmin(t, "root", users, sessions)
	if err := users.Put(&User{
		ID: mustNewUserID(t), Name: "root-passkey-holder", Role: RoleAdmin, Created: 1_700_000_000,
		Credentials: []Credential{{ID: []byte{1, 2, 3}, PublicKey: []byte{9}}},
	}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	if err := users.Put(&User{ID: mustNewUserID(t), Name: "aditi", Role: RoleViewer, Created: 1_700_000_000}); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /users status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"root", "root-passkey-holder", "aditi", "admin", "viewer", "2023-11-14"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /users body missing %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "none registered") {
		t.Errorf("GET /users body missing the zero-passkey note for aditi:\n%s", body)
	}
}

// TestUsersPageForbiddenForViewerAndAnon pins the RBAC gate at this specific
// route: a signed-in viewer gets the 403 "Admin only" panel, an anonymous
// visitor is redirected to /login — the same matrix rbac_test.go already
// pins generically across every admin route, verified again here directly
// against the real (non-placeholder) /users handler per the task brief.
func TestUsersPageForbiddenForViewerAndAnon(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/users")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusForbidden {
		t.Fatalf("GET /users as viewer status = %d, want 403, body: %s", viewerRR.Code, viewerRR.Body.String())
	}
	if !strings.Contains(viewerRR.Body.String(), "Admin only") {
		t.Errorf("GET /users viewer 403 body missing \"Admin only\":\n%s", viewerRR.Body.String())
	}

	anonRR := httptest.NewRecorder()
	h.ServeHTTP(anonRR, httptest.NewRequest(http.MethodGet, "/users", nil))
	if anonRR.Code != http.StatusFound {
		t.Fatalf("GET /users anon status = %d, want %d", anonRR.Code, http.StatusFound)
	}
	if loc := anonRR.Header().Get("Location"); loc != "/login" {
		t.Errorf("GET /users anon Location = %q, want /login", loc)
	}
}

// tokenFromEnrollLink extracts the ?token= value from an issued enroll link
// embedded in a /users/invite response body.
func tokenFromEnrollLink(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`/enroll\?token=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no enroll link found in body:\n%s", body)
	}
	return m[1]
}

// TestUsersInviteIssuesUsableEnrollLink pins the core invite contract: an
// admin issuing a token for a role gets back a rendered, single-use
// /enroll?token=... link, and that exact token actually redeems (via the
// real /enroll/begin HTTP surface) to the role the admin picked.
func TestUsersInviteIssuesUsableEnrollLink(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/users/invite", url.Values{"role": {"viewer"}, "ttl": {"1h"}}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /users/invite status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/enroll?token=") {
		t.Fatalf("POST /users/invite response missing enroll link:\n%s", body)
	}
	tok := tokenFromEnrollLink(t, body)

	beginBody, _ := json.Marshal(map[string]string{"name": "invited-viewer", "token": tok})
	beginReq := httptest.NewRequest(http.MethodPost, "/enroll/begin", strings.NewReader(string(beginBody)))
	beginReq.Header.Set("Content-Type", "application/json")
	beginRR := httptest.NewRecorder()
	h.ServeHTTP(beginRR, beginReq)
	if beginRR.Code != http.StatusOK {
		t.Fatalf("POST /enroll/begin with issued token status = %d, want 200, body: %s", beginRR.Code, beginRR.Body.String())
	}

	// The role is only actually assigned at /enroll/finish, but
	// resolveEnrollRole (which /enroll/begin calls) has already redeemed the
	// token by now; redeeming it again must fail (single-use).
	if _, err := newTokenStore(d.StateDir).Redeem(tok); err == nil {
		t.Error("issued token redeemed a second time, want single-use rejection")
	}
}

// TestUsersInviteReissueAffordance pins the "re-issue" requirement: since a
// single-use token is burned the moment /enroll/begin redeems it (or simply
// expires), an admin must be able to mint a SECOND, independently-usable
// token for the same role/ttl without the page choking on the first one
// still being outstanding.
func TestUsersInviteReissueAffordance(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	first := postForm(h, "/users/invite", url.Values{"role": {"viewer"}, "ttl": {"1h"}}, cookie, csrf)
	firstTok := tokenFromEnrollLink(t, first.Body.String())

	second := postForm(h, "/users/invite", url.Values{"role": {"viewer"}, "ttl": {"1h"}}, cookie, csrf)
	if second.Code != http.StatusOK {
		t.Fatalf("re-issue POST /users/invite status = %d, want 200, body: %s", second.Code, second.Body.String())
	}
	secondTok := tokenFromEnrollLink(t, second.Body.String())
	if secondTok == firstTok {
		t.Fatal("re-issue returned the same token as the first issue")
	}

	tokens := newTokenStore(d.StateDir)
	if _, err := tokens.Redeem(secondTok); err != nil {
		t.Errorf("re-issued token failed to redeem: %v", err)
	}
}

// TestUsersInviteRequiresCSRF pins that the invite endpoint is a CSRF-guarded
// mutation like any other admin write: no token, no mutation.
func TestUsersInviteRequiresCSRF(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/users/invite", url.Values{"role": {"viewer"}, "ttl": {"1h"}}, cookie, "" /* no CSRF */)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /users/invite without CSRF status = %d, want 403", rr.Code)
	}
}

// TestUsersChangeRoleRoundTrip pins role changes: viewer -> admin -> viewer
// both persist, checked directly against the store (not just the HTTP
// response), with two admins in play so the last-admin guard never fires.
func TestUsersChangeRoleRoundTrip(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	target := &User{ID: mustNewUserID(t), Name: "bob", Role: RoleViewer, Created: 1}
	if err := users.Put(target); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	up := postForm(h, "/users/"+target.ID+"/role", url.Values{"role": {"admin"}}, cookie, csrf)
	if up.Code != http.StatusOK {
		t.Fatalf("POST .../role (viewer->admin) status = %d, want 200, body: %s", up.Code, up.Body.String())
	}
	got, ok := users.Get(target.ID)
	if !ok || got.Role != RoleAdmin {
		t.Fatalf("after promotion, Get(%s) = %+v, %v, want role admin", target.ID, got, ok)
	}

	down := postForm(h, "/users/"+target.ID+"/role", url.Values{"role": {"viewer"}}, cookie, csrf)
	if down.Code != http.StatusOK {
		t.Fatalf("POST .../role (admin->viewer) status = %d, want 200, body: %s", down.Code, down.Body.String())
	}
	got2, ok := users.Get(target.ID)
	if !ok || got2.Role != RoleViewer {
		t.Fatalf("after demotion, Get(%s) = %+v, %v, want role viewer", target.ID, got2, ok)
	}
}

// TestUsersDemoteRefusesLastAdmin pins the lockout guard on the ROLE-CHANGE
// path: demoting the sole remaining admin to viewer must be refused, the
// same as removing them would be — otherwise the admin-only /users route
// (and every other admin route) becomes permanently unreachable, since a
// fresh bootstrap admin only happens against a fully EMPTY user store.
func TestUsersDemoteRefusesLastAdmin(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	solo, cookie, csrf := seedAdmin(t, "solo", users, sessions)

	rr := postForm(h, "/users/"+solo.ID+"/role", url.Values{"role": {"viewer"}}, cookie, csrf)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("demoting the sole admin status = %d, want 4xx, body: %s", rr.Code, rr.Body.String())
	}
	got, ok := users.Get(solo.ID)
	if !ok || got.Role != RoleAdmin {
		t.Fatalf("sole admin role after refused demotion = %+v, %v, want still admin", got, ok)
	}
}

// TestUsersRemoveWorksForNonLastAdmin pins the positive removal case: with
// two admins present, removing one succeeds and it disappears from the
// store.
func TestUsersRemoveWorksForNonLastAdmin(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	extra := &User{ID: mustNewUserID(t), Name: "second-admin", Role: RoleAdmin, Created: 1}
	if err := users.Put(extra); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	rr := postForm(h, "/users/"+extra.ID+"/remove", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST .../remove status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if _, ok := users.Get(extra.ID); ok {
		t.Error("removed user still present in store")
	}
}

// TestUsersRemoveRefusesLastAdmin pins the brief's headline guard: removing
// the sole remaining admin must be refused with an error/4xx, and the
// account must still be present afterward.
func TestUsersRemoveRefusesLastAdmin(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	solo, cookie, csrf := seedAdmin(t, "solo", users, sessions)

	rr := postForm(h, "/users/"+solo.ID+"/remove", url.Values{}, cookie, csrf)
	if rr.Code < 400 || rr.Code >= 500 {
		t.Fatalf("removing the sole admin status = %d, want 4xx, body: %s", rr.Code, rr.Body.String())
	}
	if _, ok := users.Get(solo.ID); !ok {
		t.Error("sole admin was removed despite the guard")
	}
}

// TestUsersRevokeCredentialRemovesOnlyThatOne pins scoped revocation: a user
// with two passkeys has exactly one removed by ID; the other survives
// untouched.
func TestUsersRevokeCredentialRemovesOnlyThatOne(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	keep := Credential{ID: []byte{10, 20, 30}, PublicKey: []byte{1}}
	drop := Credential{ID: []byte{40, 50, 60}, PublicKey: []byte{2}}
	target := &User{ID: mustNewUserID(t), Name: "multi-key", Role: RoleViewer, Created: 1, Credentials: []Credential{keep, drop}}
	if err := users.Put(target); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	param := credentialParam(drop.ID)
	rr := postForm(h, "/users/"+target.ID+"/credentials/"+param+"/revoke", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST .../revoke status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	got, ok := users.Get(target.ID)
	if !ok {
		t.Fatal("target user disappeared after credential revoke")
	}
	if len(got.Credentials) != 1 {
		t.Fatalf("Credentials after revoke = %d, want 1", len(got.Credentials))
	}
	if string(got.Credentials[0].ID) != string(keep.ID) {
		t.Errorf("surviving credential ID = %v, want %v (the one NOT revoked)", got.Credentials[0].ID, keep.ID)
	}
}

// TestUsersMutationsRequireAdminRole pins that the /users/* mutation routes
// are gated by requireRole(RoleAdmin, ...) exactly like GET /users itself —
// a signed-in viewer with an otherwise-valid CSRF token still gets 403.
func TestUsersMutationsRequireAdminRole(t *testing.T) {
	d, users, sessions := rbacTestDeps(t)
	h := newHandler(d)

	viewer := &User{ID: mustNewUserID(t), Name: "viewer-user", Role: RoleViewer, Created: 1}
	if err := users.Put(viewer); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	sess, err := sessions.New(viewer.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	cookie := &http.Cookie{Name: sessionCookieName, Value: sess.ID}

	rr := postForm(h, "/users/invite", url.Values{"role": {"viewer"}, "ttl": {"1h"}}, cookie, sess.CSRF)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /users/invite as viewer status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
}
