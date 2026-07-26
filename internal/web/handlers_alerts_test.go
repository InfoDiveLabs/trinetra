//go:build web

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeAlertLog writes lines (already-JSON-encoded, one per line) to
// <stateDir>/alertlog.jsonl, returning the path for AlertLogPath.
func writeAlertLog(t *testing.T, dir string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, "alertlog.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write alert log: %v", err)
	}
	return path
}

// writeAlertState writes raw JSON to <stateDir>/alerts.json, returning the
// path for AlertStatePath.
func writeAlertState(t *testing.T, dir, raw string) string {
	t.Helper()
	path := filepath.Join(dir, "alerts.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write alert state: %v", err)
	}
	return path
}

// TestAlertsPageListsLogEvents pins the core TDD obligation: a populated
// alert log renders as history rows on GET /alerts.
func TestAlertsPageListsLogEvents(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertLogPath = writeAlertLog(t, d.StateDir,
		`{"time":1700000000,"key":"disk:/","title":"Filesystem almost full","severity":"critical","kind":"fire","source":"disk:/","delivered":[{"channel":"Telegram","ok":true}]}`,
		`{"time":1700000100,"key":"disk:/","title":"Filesystem almost full","severity":"critical","kind":"recover","source":"disk:/"}`,
	)
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

// TestAlertsPageMissingLogRendersEmptyNot500 pins the "missing/garbage file
// -> empty, never 500" requirement for a log path that doesn't exist.
func TestAlertsPageMissingLogRendersEmptyNot500(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertLogPath = filepath.Join(d.StateDir, "does-not-exist.jsonl")
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

// TestAlertsPageGarbageLogRendersEmptyNot500 pins the same contract for a
// log file that exists but contains garbage.
func TestAlertsPageGarbageLogRendersEmptyNot500(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertLogPath = writeAlertLog(t, d.StateDir, "not json at all {{{")
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
// unacked alert in AlertStatePath renders with an Ack button for an admin.
func TestAlertsPageListsActiveAlerts(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1700000000,"reason":"disk:/ = 91.0 >= threshold 90.0"}}}`)
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
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1700000000,"reason":"x"}}}`)
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
// ack flips Acked/AckedAt in the on-disk AlertState and writes an audit
// record.
func TestAlertsAckRoundTripsToAlertState(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1700000000,"reason":"x"}}}`)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	param := credentialParam([]byte("disk:/"))
	rr := postForm(h, "/alerts/"+param+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}

	state := loadAckAlertState(d.AlertStatePath)
	a, ok := state.Active["disk:/"]
	if !ok {
		t.Fatal("disk:/ disappeared from AlertState")
	}
	if !a.Acked || a.AckedAt == 0 {
		t.Errorf("active alert = %+v, want Acked=true and AckedAt set", a)
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
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{}}`)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/alerts/"+credentialParam([]byte("ghost"))+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestAlertsAckMissingStateFileNotFound pins that a not-yet-existing
// AlertStatePath (alerting has never fired) degrades to "no active alert"
// (404), not a 500.
func TestAlertsAckMissingStateFileNotFound(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.AlertStatePath = filepath.Join(d.StateDir, "does-not-exist.json")
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
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1,"reason":"x"}}}`)
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
	d.AlertStatePath = writeAlertState(t, d.StateDir, `{"active":{"disk:/":{"since":1,"reason":"x"}}}`)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, cookie, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	state := loadAckAlertState(d.AlertStatePath)
	if state.Active["disk:/"].Acked {
		t.Error("alert should not have been acked without a valid CSRF token")
	}
}
