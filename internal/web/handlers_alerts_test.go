package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestAlertsPageListsLogEvents pins the core TDD obligation: a populated
// alert log (served through the control socket, Deps.API.AlertHistory)
// renders as history rows on GET /alerts.
func TestAlertsPageListsLogEvents(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{history: []core.AlertRecord{
		{Time: 1700000000, Key: "disk:/", Title: "Filesystem almost full", Severity: "critical", Kind: "fire", Source: "disk:/", Delivered: true, DeliveredTo: []string{"Telegram"}},
		{Time: 1700000100, Key: "disk:/", Title: "Filesystem almost full", Severity: "critical", Kind: "recover", Source: "disk:/"},
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/alerts")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Filesystem almost full", "Telegram", "recovered", "fired"} {
		if !strings.Contains(body, want) {
			t.Errorf("alerts page missing %q:\n%s", want, body)
		}
	}
}

// TestAlertsPageMissingLogRendersEmptyNot500 pins the "no history -> empty,
// never 500" requirement when the control socket reports an empty history.
func TestAlertsPageMissingLogRendersEmptyNot500(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/alerts")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "No alert history yet") {
		t.Errorf("body missing empty-state message:\n%s", rr.Body.String())
	}
}

// TestAlertsPageGarbageLogRendersEmptyNot500 pins the same contract when the
// control socket read fails (degrades to nil history), never a 500.
func TestAlertsPageGarbageLogRendersEmptyNot500(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{histErr: errTestActiveAlerts}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/alerts")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "No alert history yet") {
		t.Errorf("body missing empty-state message:\n%s", rr.Body.String())
	}
}

// TestAlertsPageListsActiveAlerts pins the "Firing" panel: an active,
// unacked alert (via the control socket) renders with an Ack button for an
// admin.
func TestAlertsPageListsActiveAlerts(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1700000000, Source: "disk:/ = 91.0 >= threshold 90.0"}}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/alerts")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"disk:/", "threshold 90.0", `action="/alerts/`, ">Ack<"} {
		if !strings.Contains(body, want) {
			t.Errorf("alerts page missing %q:\n%s", want, body)
		}
	}
}

// TestAlertsPageViewerHasNoAckButton pins that a viewer sees active alerts
// but no Ack action (ack is admin-only).
func TestAlertsPageViewerHasNoAckButton(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1700000000, Source: "x"}}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/alerts")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), ">Ack<") {
		t.Error("viewer should not see an Ack button")
	}
}

// TestAlertsAckRoundTripsToAlertState pins the core ack obligation: POSTing
// ack acks the named active alert THROUGH the control socket
// (Deps.API.AckAlert) and writes an audit record.
func TestAlertsAckRoundTripsToAlertState(t *testing.T) {
	d, _, _ := configTestDeps(t)
	var ackedKey string
	ackCalled := false
	d.API = fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1700000000, Source: "x"}},
		ackAlert: func(k string) error { ackCalled = true; ackedKey = k; return nil },
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	param := credentialParam([]byte("disk:/"))
	rr := postForm(h, "/alerts/"+param+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	if !ackCalled {
		t.Fatal("AckAlert was never called through the control socket")
	}
	if ackedKey != "disk:/" {
		t.Errorf("AckAlert called with key %q, want %q", ackedKey, "disk:/")
	}

	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "alert.ack" && r.Key == "disk:/" {
			found = true
			if r.User != "root" {
				t.Errorf("audit user = %q, want root", r.User)
			}
		}
	}
	if !found {
		t.Errorf("no alert.ack audit record, got: %+v", recs)
	}
}

// TestAlertsAckUnknownKeyNotFound pins that acking a key with no active
// alert 404s rather than silently creating one.
func TestAlertsAckUnknownKeyNotFound(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/alerts/"+credentialParam([]byte("ghost"))+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestAlertsAckMissingStateFileNotFound pins that when alerting has never
// fired (no active alerts over the socket) an ack degrades to "no active
// alert" (404), not a 500.
func TestAlertsAckMissingStateFileNotFound(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestAlertsRouteViewerGatedAckAdminOnly pins RBAC: /alerts itself is
// viewer+ (anon redirects to /login), while ack is admin-only (viewer gets
// 403).
func TestAlertsRouteViewerGatedAckAdminOnly(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	anonRR := httptest.NewRecorder()
	h.ServeHTTP(anonRR, httptest.NewRequest(http.MethodGet, "/alerts", nil))
	if anonRR.Code != http.StatusFound {
		t.Errorf("GET /alerts anon status = %d, want 302", anonRR.Code)
	}

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/alerts")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusOK {
		t.Errorf("GET /alerts as viewer status = %d, want 200", viewerRR.Code)
	}

	_, viewerCookie, viewerCSRF := seedViewerWithCSRF(t, users, sessions)
	ackRR := postForm(h, "/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, viewerCookie, viewerCSRF)
	if ackRR.Code != http.StatusForbidden {
		t.Errorf("POST ack as viewer status = %d, want 403, body: %s", ackRR.Code, ackRR.Body.String())
	}
}

// seedViewerWithCSRF mirrors seedAdmin (handlers_users_test.go) for a
// viewer-role account.
func seedViewerWithCSRF(t *testing.T, users UserStore, sessions SessionStore) (*User, *http.Cookie, string) {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: "viewer", Role: RoleViewer, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed viewer Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	return u, &http.Cookie{Name: sessionCookieName, Value: sess.ID}, sess.CSRF
}

// TestAlertsAckRequiresCSRF pins that an admin POST without a valid CSRF
// token is rejected, matching every other mutation route.
func TestAlertsAckRequiresCSRF(t *testing.T) {
	d, _, _ := configTestDeps(t)
	ackCalled := false
	d.API = fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}},
		ackAlert: func(k string) error { ackCalled = true; return nil },
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, cookie, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	if ackCalled {
		t.Error("alert should not have been acked without a valid CSRF token")
	}
}
