package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestUpdatesPageAdminGated pins RBAC: a viewer gets 403, an admin gets 200
// with the available version and an Apply form carrying a CSRF hidden
// field -- mirroring TestConfigRoutesAreAdminGated.
func TestUpdatesPageAdminGated(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{updateStatus: core.UpdateStatusView{Running: "0.5.0", Available: "0.5.1"}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/updates")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusForbidden {
		t.Errorf("GET /updates as viewer status = %d, want 403", viewerRR.Code)
	}

	adminReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	adminRR := httptest.NewRecorder()
	h.ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusOK {
		t.Fatalf("GET /updates as admin status = %d, want 200, body: %s", adminRR.Code, adminRR.Body.String())
	}
	body := adminRR.Body.String()
	if !strings.Contains(body, "0.5.1") {
		t.Errorf("updates page missing available version 0.5.1:\n%s", body)
	}
	values := formValuesForButton(t, body, "do", "apply")
	if values.Get("csrf_token") == "" {
		t.Error("Apply form missing a non-empty csrf_token hidden field")
	}
	if values.Get("version") != "0.5.1" {
		t.Errorf("Apply form version field = %q, want 0.5.1", values.Get("version"))
	}
}

// TestUpdatesPageRendersInProgressAndLastError pins fix round 1's Ruling
// R10 web-side requirement: /updates must show a banner while a background
// apply/rollback is running (Status.InProgress) and, once it's finished, the
// daemon's own last-error text (Status.LastError) -- rendered through
// html/template's normal auto-escaping (never inserted unescaped), and
// suppressed while InProgress is still true (a stale error from a PREVIOUS
// attempt must not be shown as if it were this one's outcome).
func TestUpdatesPageRendersInProgressAndLastError(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{updateStatus: core.UpdateStatusView{
		Running:    "0.5.0",
		InProgress: true,
		LastError:  "update: <script>alert(1)</script> boom",
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Update in progress") {
		t.Errorf("updates page must show an in-progress banner while Status.InProgress is true:\n%s", body)
	}
	if strings.Contains(body, "Last update error") {
		t.Errorf("updates page must not show LastError while InProgress is still true (stale error from a previous attempt):\n%s", body)
	}

	d2 := enrollTestDeps(t)
	d2.StateDir = d.StateDir
	d2.API = fakeAPI{updateStatus: core.UpdateStatusView{
		Running:   "0.5.0",
		LastError: "update: <script>alert(1)</script> boom",
	}}
	h2 := newHandler(d2)
	req2 := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	rr2 := httptest.NewRecorder()
	h2.ServeHTTP(rr2, req2)
	body2 := rr2.Body.String()
	if strings.Contains(body2, "Update in progress") {
		t.Errorf("updates page must not show the in-progress banner once InProgress is false:\n%s", body2)
	}
	if !strings.Contains(body2, "Last update error") {
		t.Errorf("updates page must show LastError once the background operation has finished:\n%s", body2)
	}
	if strings.Contains(body2, "<script>alert(1)</script>") {
		t.Errorf("LastError must be HTML-escaped, not inserted raw:\n%s", body2)
	}
	if !strings.Contains(body2, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("LastError's escaped form not found in the page:\n%s", body2)
	}
}

// TestUpdatesPageAnonRedirectsToLogin mirrors the anon half of
// TestConfigRoutesAreAdminGated for /updates.
func TestUpdatesPageAnonRedirectsToLogin(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{}
	h := newHandler(d)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/updates", nil))
	if rr.Code != http.StatusFound {
		t.Errorf("GET /updates anon status = %d, want 302", rr.Code)
	}
}

// TestUpdatesApplyCallsAPIAndRedirects pins the success path: a valid admin
// POST /updates/apply calls core.API.UpdateApply with the posted version and
// redirects 303 to /updates?flash=update-started.
func TestUpdatesApplyCallsAPIAndRedirects(t *testing.T) {
	var gotVersion string
	var applyCalled bool
	d := enrollTestDeps(t)
	d.API = fakeAPI{
		updateStatus: core.UpdateStatusView{Running: "0.5.0", Available: "0.5.1"},
		updateApply: func(ctx context.Context, version string) error {
			applyCalled = true
			gotVersion = version
			return nil
		},
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	getReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	values := formValuesForButton(t, getRR.Body.String(), "do", "apply")

	rr := postForm(h, "/updates/apply", values, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/updates?flash=update-started" {
		t.Errorf("redirect Location = %q, want /updates?flash=update-started", loc)
	}
	if !applyCalled {
		t.Fatal("core.API.UpdateApply was never called")
	}
	if gotVersion != "0.5.1" {
		t.Errorf("UpdateApply called with version %q, want 0.5.1", gotVersion)
	}
}

// TestUpdatesApplyRequiresCSRF pins that a signed-in admin POST without a
// valid CSRF token is rejected with 403 and the fake is never called,
// matching every other mutation route (TestConfigSaveRequiresCSRF).
func TestUpdatesApplyRequiresCSRF(t *testing.T) {
	var applyCalled bool
	d := enrollTestDeps(t)
	d.API = fakeAPI{
		updateStatus: core.UpdateStatusView{Running: "0.5.0", Available: "0.5.1"},
		updateApply: func(ctx context.Context, version string) error {
			applyCalled = true
			return nil
		},
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/updates/apply", url.Values{"version": {"0.5.1"}}, cookie, "" /* no CSRF token */)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	if applyCalled {
		t.Error("core.API.UpdateApply was called despite a missing CSRF token")
	}
}

// TestUpdatesApplyViewerForbidden pins that a viewer session cannot POST
// /updates/apply even with a form the page itself would never show them
// (RoleAdmin gate runs before requireCSRF, mirroring updatesMutation's
// composition).
func TestUpdatesApplyViewerForbidden(t *testing.T) {
	var applyCalled bool
	d := enrollTestDeps(t)
	d.API = fakeAPI{
		updateApply: func(ctx context.Context, version string) error {
			applyCalled = true
			return nil
		},
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedViewer(t, "bob", users, sessions)

	rr := postForm(h, "/updates/apply", url.Values{"version": {"0.5.1"}}, cookie, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	if applyCalled {
		t.Error("core.API.UpdateApply was called for a viewer session")
	}
}

// TestUpdatesCheckCallsAPIAndRedirects pins POST /updates/check's success
// path.
func TestUpdatesCheckCallsAPIAndRedirects(t *testing.T) {
	var checkCalled bool
	d := enrollTestDeps(t)
	fake := &fakeUpdateAPI{
		status:     core.UpdateStatusView{Running: "0.5.0"},
		checkCalls: &checkCalled,
	}
	d.API = fake
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/updates/check", nil, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/updates?flash=update-checked" {
		t.Errorf("redirect Location = %q, want /updates?flash=update-checked", loc)
	}
	if !checkCalled {
		t.Fatal("core.API.UpdateCheck was never called")
	}
}

// TestUpdatesCheckErrorFlashesUpdateError pins the failure path: a real
// UpdateCheck error (not the "already installed" case) redirects with the
// fixed "update-error" code, never the error's own text.
func TestUpdatesCheckErrorFlashesUpdateError(t *testing.T) {
	d := enrollTestDeps(t)
	fake := &fakeUpdateAPI{checkErr: errors.New("update: signature does not verify against any trusted key")}
	d.API = fake
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/updates/check", nil, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/updates?flash=update-error" {
		t.Errorf("redirect Location = %q, want /updates?flash=update-error", loc)
	}
}

// TestUpdatesRollbackOnlyOfferedWithPrevious pins the "Roll back" button's
// conditional rendering: absent when Status.Previous == "", present
// (carrying CSRF) when it's set.
func TestUpdatesRollbackOnlyOfferedWithPrevious(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{updateStatus: core.UpdateStatusView{Running: "0.5.1"}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), `action="/updates/rollback"`) {
		t.Errorf("updates page must not offer rollback with no Previous build:\n%s", rr.Body.String())
	}

	d2 := enrollTestDeps(t)
	d2.StateDir = d.StateDir
	d2.API = fakeAPI{updateStatus: core.UpdateStatusView{Running: "0.5.1", Previous: "0.5.0"}}
	h2 := newHandler(d2)
	req2 := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/updates")
	rr2 := httptest.NewRecorder()
	h2.ServeHTTP(rr2, req2)
	body2 := rr2.Body.String()
	if !strings.Contains(body2, `action="/updates/rollback"`) {
		t.Errorf("updates page must offer rollback once Previous is set:\n%s", body2)
	}
	values := formValuesForButton(t, body2, "do", "rollback")
	if values.Get("csrf_token") == "" {
		t.Error("rollback form missing a non-empty csrf_token hidden field")
	}
}

// TestUpdatesRollbackCallsAPIAndRedirects pins POST /updates/rollback's
// success path.
func TestUpdatesRollbackCallsAPIAndRedirects(t *testing.T) {
	var rollbackCalled bool
	d := enrollTestDeps(t)
	fake := &fakeUpdateAPI{
		status:        core.UpdateStatusView{Running: "0.5.1", Previous: "0.5.0"},
		rollbackCalls: &rollbackCalled,
	}
	d.API = fake
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/updates/rollback", nil, cookie, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/updates?flash=rollback-started" {
		t.Errorf("redirect Location = %q, want /updates?flash=rollback-started", loc)
	}
	if !rollbackCalled {
		t.Fatal("core.API.UpdateRollback was never called")
	}
}

// fakeUpdateAPI is a minimal core.API test double focused on the four
// self-update methods (embedding fakeAPI for every other method), used where
// a test needs to observe UpdateCheck/UpdateRollback being called -- fakeAPI
// itself only exposes optional func fields for UpdateApply/UpdateRollback,
// not UpdateCheck, and its UpdateCheck always returns (f.updateStatus,
// f.updateCheckErr) with no call-observation hook.
type fakeUpdateAPI struct {
	fakeAPI
	status     core.UpdateStatusView
	checkErr   error
	checkCalls *bool

	rollbackErr   error
	rollbackCalls *bool
}

func (f *fakeUpdateAPI) UpdateStatus() (core.UpdateStatusView, error) { return f.status, nil }

func (f *fakeUpdateAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	if f.checkCalls != nil {
		*f.checkCalls = true
	}
	return f.status, f.checkErr
}

func (f *fakeUpdateAPI) UpdateRollback() error {
	if f.rollbackCalls != nil {
		*f.rollbackCalls = true
	}
	return f.rollbackErr
}

var _ core.API = (*fakeUpdateAPI)(nil)

// TestUpdatesApplyAndRollbackBusyFlash is R16: when another apply/rollback/
// install already holds the host's update lock (from the CLI, the socket or
// another browser), the web actions say so plainly with the fixed
// "update-busy" code instead of a generic failure.
func TestUpdatesApplyAndRollbackBusyFlash(t *testing.T) {
	busy := errors.New("update: an update is already in progress (see trinetra update status)")
	d := enrollTestDeps(t)
	d.API = &fakeUpdateAPI{
		fakeAPI:     fakeAPI{updateApply: func(context.Context, string) error { return busy }},
		status:      core.UpdateStatusView{Running: "0.5.0", Available: "0.5.1", Previous: "0.4.9"},
		rollbackErr: busy,
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	for _, path := range []string{"/updates/apply", "/updates/rollback"} {
		rr := postForm(h, path, url.Values{"version": {"0.5.1"}}, cookie, csrf)
		if loc := rr.Header().Get("Location"); loc != "/updates?flash=update-busy" {
			t.Errorf("%s: Location = %q, want /updates?flash=update-busy", path, loc)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/updates?flash=update-busy", nil)
	if text, isErr := resolveUpdatesFlash(req); !isErr || !strings.Contains(text, "already in progress") {
		t.Fatalf("update-busy flash = %q, %v", text, isErr)
	}
}
