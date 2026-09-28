package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
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

// TestAlertsUnackRoundTripsToAlertState is TestAlertsAckRoundTripsToAlertState's
// unack counterpart (self scope): POSTing unack for an already-acked active
// alert calls Deps.API.UnackAlert and re-renders the page in place at 200
// (no redirect -- self scope is unchanged by task C6), plus writes an
// "alert.unack" audit record.
func TestAlertsUnackRoundTripsToAlertState(t *testing.T) {
	d, _, _ := configTestDeps(t)
	var unackedKey string
	unackCalled := false
	d.API = fakeAPI{
		active:     []core.AlertRecord{{Key: "disk:/", Time: 1700000000, Source: "x", Acked: true}},
		unackAlert: func(k string) error { unackCalled = true; unackedKey = k; return nil },
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	param := credentialParam([]byte("disk:/"))
	rr := postForm(h, "/alerts/"+param+"/unack", nil, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !unackCalled {
		t.Fatal("UnackAlert was never called through the control socket")
	}
	if unackedKey != "disk:/" {
		t.Errorf("UnackAlert called with key %q, want disk:/", unackedKey)
	}
	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "alert.unack" && r.Key == "disk:/" {
			found = true
			if r.User != "root" {
				t.Errorf("audit user = %q, want root", r.User)
			}
		}
	}
	if !found {
		t.Errorf("no alert.unack audit record, got: %+v", recs)
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

// ---- task C6: remote node ack/unack ----------------------------------------

// nodeScopedAlertsDeps builds a master Deps with a single fleet node
// (child1) whose roster State is childState -- "online" for a connected
// node, anything else (e.g. "down") for a disconnected one -- and child as
// its core.API, exactly like node_scope_test.go's masterFleetWithChild/
// nodeScopedDeps but with a caller-controlled State so these tests can
// exercise both sides of nodeConnectedFor's "online" check.
func nodeScopedAlertsDeps(t *testing.T, childState string, child fakeAPI) Deps {
	t.Helper()
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Self: true, State: "online"},
			{ID: "child1", Name: "child-one", State: childState},
		},
	}
	return fleetTestDeps(t, masterFakeAPI(fleet, map[string]core.API{"child1": child}))
}

// TestNodeScopedAlertsAckSucceedsWhenNodeConnected pins the core remote-ack
// obligation (task C6 ruling 1): a connected node's Ack button is enabled,
// and clicking it (via formValuesForButton, the browser-faithful helper)
// calls THAT node's own core.API.AckAlert -- not the master's.
func TestNodeScopedAlertsAckSucceedsWhenNodeConnected(t *testing.T) {
	var ackCalled bool
	var ackedKey string
	child := fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}},
		ackAlert: func(k string) error { ackCalled = true; ackedKey = k; return nil },
	}
	d := nodeScopedAlertsDeps(t, "online", child)
	body, h, cookie := fleetAdminSessionGet(t, d, "/n/child1/alerts")
	if strings.Contains(body, "node is not connected") {
		t.Fatalf("connected node's alerts page still shows the disabled reason:\n%s", body)
	}

	values := formValuesForButton(t, body, "", "")
	rr := postForm(h, "/n/child1/alerts/"+credentialParam([]byte("disk:/"))+"/ack", values, cookie, values.Get("csrf_token"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/n/child1/alerts" {
		t.Errorf("redirect Location = %q, want /n/child1/alerts", loc)
	}
	if !ackCalled {
		t.Fatal("child1's AckAlert was never called")
	}
	if ackedKey != "disk:/" {
		t.Errorf("AckAlert called with key %q, want disk:/", ackedKey)
	}
}

// TestNodeScopedAlertsAckDisabledWhenNodeOffline pins the disabled side: an
// offline node's active-alert row has no live Ack form at all, only a
// disabled button carrying the fixed "node is not connected" reason.
func TestNodeScopedAlertsAckDisabledWhenNodeOffline(t *testing.T) {
	child := fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}}}
	d := nodeScopedAlertsDeps(t, "down", child)
	body, _, _ := fleetAdminSessionGet(t, d, "/n/child1/alerts")

	if !strings.Contains(body, `disabled title="node is not connected">node is not connected<`) {
		t.Errorf("offline node's alerts page missing the disabled Ack button:\n%s", body)
	}
	if strings.Contains(body, `action="/n/child1/alerts/`) {
		t.Errorf("offline node's alerts page must not render a live Ack form action:\n%s", body)
	}
}

// TestNodeScopedAlertsAckFailureFlashesRemoteOffline pins the redirect+flash
// contract for a remote ack that fails: a race where the node drops between
// page load and the POST (AckAlert now returning "node is not connected")
// redirects back to the node-scoped page with ?flash=remote-offline, never
// free-form error text.
func TestNodeScopedAlertsAckFailureFlashesRemoteOffline(t *testing.T) {
	child := fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}},
		ackAlert: func(string) error { return errors.New(nodeNotConnectedReason) },
	}
	// Roster says "online" so the button renders live -- the failure is
	// simulated purely via AckAlert's own returned error, exercising the
	// server-side race/error path independent of the client-side disable.
	d := nodeScopedAlertsDeps(t, "online", child)
	body, h, cookie := fleetAdminSessionGet(t, d, "/n/child1/alerts")
	values := formValuesForButton(t, body, "", "")

	rr := postForm(h, "/n/child1/alerts/"+credentialParam([]byte("disk:/"))+"/ack", values, cookie, values.Get("csrf_token"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303, body: %s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/n/child1/alerts?flash=remote-offline" {
		t.Errorf("redirect Location = %q, want .../n/child1/alerts?flash=remote-offline", loc)
	}
}

// TestNodeScopedAlertsAckPostMissingCSRFForbidden pins that a node-scoped
// ack POST still requires CSRF -- withNodeRouter's new POST exception
// (node_scope.go) re-dispatches to the SAME admin+CSRF-gated route, it
// doesn't bypass either gate.
func TestNodeScopedAlertsAckPostMissingCSRFForbidden(t *testing.T) {
	child := fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}},
		ackAlert: func(string) error { t.Fatal("AckAlert must not be called without a valid CSRF token"); return nil },
	}
	d := nodeScopedAlertsDeps(t, "online", child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/n/child1/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, cookie, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNodeScopedAlertsAckPostViewerForbidden pins that a node-scoped ack
// POST is still admin-only.
func TestNodeScopedAlertsAckPostViewerForbidden(t *testing.T) {
	child := fakeAPI{
		active:   []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "x"}},
		ackAlert: func(string) error { t.Fatal("AckAlert must not be called by a viewer"); return nil },
	}
	d := nodeScopedAlertsDeps(t, "online", child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedViewerWithCSRF(t, users, sessions)

	rr := postForm(h, "/n/child1/alerts/"+credentialParam([]byte("disk:/"))+"/ack", nil, cookie, csrf)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNodeScopedPostToOtherPathsStillRefused pins that the new POST
// exception in withNodeRouter is narrow: a node-scoped POST to any path
// OTHER than exactly /alerts/{key}/ack|unack still 404s, admin+CSRF or not.
func TestNodeScopedPostToOtherPathsStillRefused(t *testing.T) {
	child := fakeAPI{}
	d := nodeScopedAlertsDeps(t, "online", child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	for _, target := range []string{
		"/n/child1/api/container/logs",
		"/n/child1/alerts",
		"/n/child1/alerts/somekey/ack/extra",
		"/n/child1/fleet/managed",
	} {
		t.Run(target, func(t *testing.T) {
			rr := postForm(h, target, nil, cookie, csrf)
			if rr.Code != http.StatusNotFound {
				t.Errorf("POST %s status = %d, want 404, body: %s", target, rr.Code, rr.Body.String())
			}
		})
	}
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
