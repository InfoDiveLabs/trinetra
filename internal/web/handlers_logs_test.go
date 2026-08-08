package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestContainerLogsEndpointAdmin pins GET /api/container/logs for an admin: it
// returns the daemon's log snapshot as plain text.
func TestContainerLogsEndpointAdmin(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{containerLogs: "hello from web\nsecond line\n"}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/api/container/logs?name=web&tail=50")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if body := rr.Body.String(); !strings.Contains(body, "hello from web") {
		t.Errorf("body = %q", body)
	}
}

// TestContainerLogsEndpointGating pins the failure/permission contract: a
// viewer is refused, a missing name is a 400, and a daemon error is a 404.
func TestContainerLogsEndpointGating(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{logErr: errors.New("no such container \"ghost\"")}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	// Viewer is denied the admin route (not 200).
	vReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/api/container/logs?name=web")
	vrr := httptest.NewRecorder()
	h.ServeHTTP(vrr, vReq)
	if vrr.Code == http.StatusOK {
		t.Errorf("viewer got 200 on admin logs route, want denied")
	}

	// Missing name -> 400.
	mReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/api/container/logs")
	mrr := httptest.NewRecorder()
	h.ServeHTTP(mrr, mReq)
	if mrr.Code != http.StatusBadRequest {
		t.Errorf("missing name status = %d, want 400", mrr.Code)
	}

	// Unknown container (daemon error) -> 404.
	eReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/api/container/logs?name=ghost")
	err := httptest.NewRecorder()
	h.ServeHTTP(err, eReq)
	if err.Code != http.StatusNotFound {
		t.Errorf("unknown container status = %d, want 404", err.Code)
	}
}
