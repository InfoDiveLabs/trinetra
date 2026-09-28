package web

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetAlertingViewerPost issues a viewer-signed-in, form-encoded POST
// against d's handler with no CSRF token -- for the routes this package
// deliberately leaves CSRF-free at the viewer floor (POST
// /fleet/alerting/test, which only ever dry-runs RouteTest and never
// mutates the saved config).
func fleetAlertingViewerPost(t *testing.T, d Deps, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	u := &User{ID: mustNewUserID(t), Name: "viewer-user", Role: RoleViewer, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed viewer Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// fleetAlertingSaveForm returns a minimal, individually valid structured-
// form POST body for one route (tag=web -> policy p1) and one policy (p1,
// one immediate step to every channel), op=save -- the shared fixture every
// save-path test in this file starts from (mutating a copy's fields as
// needed).
func fleetAlertingSaveForm(version string) url.Values {
	return url.Values{
		"mode":                     {"form"},
		"op":                       {"save"},
		"version":                  {version},
		"route_count":              {"1"},
		"policy_count":             {"1"},
		"rule_count":               {"0"},
		"default_policy":           {"p1"},
		"route_0_name":             {"r1"},
		"route_0_policy":           {"p1"},
		"route_0_group_by":         {""},
		"route_0_matcher_count":    {"1"},
		"route_0_matcher_0_tag":    {"web"},
		"policy_0_name":            {"p1"},
		"policy_0_repeat_every":    {""},
		"policy_0_send_resolved":   {"1"},
		"policy_0_step_count":      {"1"},
		"policy_0_step_0_after":    {"0s"},
		"policy_0_step_0_channels": {"*"},
	}
}

// TestFleetAlertingSoloDaemon404s pins "master only, else 404" for the page
// itself on a non-master daemon.
func TestFleetAlertingSoloDaemon404s(t *testing.T) {
	d := fleetSoloDeps(t)
	rr := fleetGetAsRole(t, d, RoleAdmin, "/fleet/alerting")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /fleet/alerting on solo status = %d, want 404", rr.Code)
	}
}

// TestFleetAlertingPageRendersDefaultConfig pins the fresh/never-saved
// state: no routes, no policies configured yet.
func TestFleetAlertingPageRendersDefaultConfig(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	rr := fleetAdminGetAsRole(t, d, RoleAdmin, "/fleet/alerting")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/alerting status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"No routes configured", "No policies configured", "Route tester", "Save routing config"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/alerting: missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestFleetAlertingViewerReadOnly pins the viewer floor: GET succeeds
// (read-only), but no edit forms render (no "Add route"/"Save routing
// config"/"Save JSON" controls) -- only the route tester stays a live form.
func TestFleetAlertingViewerReadOnly(t *testing.T) {
	fleet := &fakeFleet{alertingCfg: core.AlertingConfig{
		Routes:        []core.Route{{Name: "r1", Matchers: []core.Matcher{{Tag: "web"}}, Policy: "p1"}},
		Policies:      []core.Policy{{Name: "p1", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}},
		DefaultPolicy: "p1",
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/alerting")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/alerting as viewer status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "r1") || !strings.Contains(body, "p1") {
		t.Errorf("GET /fleet/alerting as viewer: missing route/policy data\nbody:\n%s", body)
	}
	for _, unwanted := range []string{"Save routing config", "Add route", "Add policy", "Add rule", "Save JSON"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("GET /fleet/alerting as viewer: found admin-only control %q, want read-only page\nbody:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(body, "Test route") {
		t.Errorf("GET /fleet/alerting as viewer: missing the route tester form\nbody:\n%s", body)
	}
}

// TestFleetAlertingPostViewerForbidden pins RBAC: a viewer POSTing the save
// route gets 403 before any master/CSRF/validation logic ever runs.
func TestFleetAlertingPostViewerForbidden(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodPost, "/fleet/alerting")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST /fleet/alerting as viewer = %d, want 403", rr.Code)
	}
}

// TestFleetAlertingMissingCSRFForbidden pins requireCSRF on the save route:
// an admin session with no CSRF token gets 403.
func TestFleetAlertingMissingCSRFForbidden(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)
	rr := postForm(h, "/fleet/alerting", fleetAlertingSaveForm("0"), cookie, "")
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST /fleet/alerting with no CSRF token = %d, want 403", rr.Code)
	}
}

// TestFleetAlertingFormSaveRoundTrip pins the structured form's save path
// end to end: SetAlerting is called with the converted core.AlertingConfig,
// the actor is the SIGNED-IN admin's own name (not a placeholder), an audit
// record is written, and the re-rendered page reflects the saved (now
// version-bumped) config.
func TestFleetAlertingFormSaveRoundTrip(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAdminPost(t, d, "/fleet/alerting", fleetAlertingSaveForm("0"))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 1 {
		t.Fatalf("SetAlerting calls = %d, want 1", fleet.setAlertingCalls)
	}
	if fleet.setAlertingActor != "root" {
		t.Errorf("SetAlerting actor = %q, want %q (the signed-in web user)", fleet.setAlertingActor, "root")
	}
	got := fleet.setAlertingCfg
	if len(got.Routes) != 1 || got.Routes[0].Name != "r1" || got.Routes[0].Policy != "p1" {
		t.Errorf("SetAlerting cfg.Routes = %+v, want one route r1/p1", got.Routes)
	}
	if len(got.Policies) != 1 || got.Policies[0].Name != "p1" || len(got.Policies[0].Steps) != 1 {
		t.Errorf("SetAlerting cfg.Policies = %+v, want one policy p1 with one step", got.Policies)
	}
	if got.DefaultPolicy != "p1" {
		t.Errorf("SetAlerting cfg.DefaultPolicy = %q, want p1", got.DefaultPolicy)
	}

	body := rr.Body.String()
	if !strings.Contains(body, "alerting config saved") {
		t.Errorf("POST /fleet/alerting: missing saved flash\nbody:\n%s", body)
	}
	if !strings.Contains(body, "r1") || !strings.Contains(body, "p1") {
		t.Errorf("POST /fleet/alerting: re-rendered page missing saved route/policy\nbody:\n%s", body)
	}

	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "fleet.alerting.set" {
			found = true
		}
	}
	if !found {
		t.Errorf("no fleet.alerting.set audit record found: %+v", recs)
	}
}

// TestFleetAlertingAddRouteOpDoesNotSave pins applyAlertingOp's "reshape,
// never save" contract: an op=add_route submit re-renders the page with a
// second route row but never calls SetAlerting at all.
func TestFleetAlertingAddRouteOpDoesNotSave(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	form := fleetAlertingSaveForm("0")
	form.Set("op", "add_route")
	rr := fleetAdminPost(t, d, "/fleet/alerting", form)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=add_route status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 0 {
		t.Errorf("SetAlerting calls = %d, want 0 (add_route must not save)", fleet.setAlertingCalls)
	}
	if !strings.Contains(rr.Body.String(), ">Route 1<") {
		t.Errorf("POST /fleet/alerting op=add_route: expected a second route row (\"Route 1\")\nbody:\n%s", rr.Body.String())
	}
}

// TestFleetAlertingJSONSave pins the "edit as JSON" mode: the posted
// textarea is unmarshaled straight into a core.AlertingConfig and saved.
func TestFleetAlertingJSONSave(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	raw := `{"version":0,"routes":[{"name":"jr","matchers":[{"tag":"web"}],"policy":"jp"}],"policies":[{"name":"jp","steps":[{"after":"0s","channels":["*"]}]}],"default_policy":"jp"}`
	rr := fleetAdminPost(t, d, "/fleet/alerting", url.Values{"mode": {"json"}, "json_config": {raw}})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting mode=json status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 1 {
		t.Fatalf("SetAlerting calls = %d, want 1", fleet.setAlertingCalls)
	}
	if fleet.setAlertingActor != "root" {
		t.Errorf("SetAlerting actor = %q, want %q", fleet.setAlertingActor, "root")
	}
	if len(fleet.setAlertingCfg.Routes) != 1 || fleet.setAlertingCfg.Routes[0].Name != "jr" {
		t.Errorf("SetAlerting cfg.Routes = %+v, want one route jr", fleet.setAlertingCfg.Routes)
	}
}

// TestFleetAlertingValidationErrorKeepsInputInline pins global-
// constraints.md's ruling: a rejected save re-renders at 400 with the
// backend's message inline next to the field it names (a route, here), and
// every posted value preserved.
func TestFleetAlertingValidationErrorKeepsInputInline(t *testing.T) {
	fleet := &fakeFleet{setAlertingErr: fmt.Errorf(`route "r1": unknown policy "p1"`)}
	d := fleetAdminDeps(t, fleet)
	form := fleetAlertingSaveForm("0")
	form.Set("route_0_name", "weird<name>")
	rr := fleetAdminPost(t, d, "/fleet/alerting", form)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/alerting (rejected) status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(html.UnescapeString(body), `unknown policy "p1"`) {
		t.Errorf("POST /fleet/alerting (rejected): missing backend error text\nbody:\n%s", body)
	}
	if !strings.Contains(body, "weird&lt;name&gt;") {
		t.Errorf("POST /fleet/alerting (rejected): posted route name not preserved/escaped\nbody:\n%s", body)
	}
	if strings.Contains(body, "weird<name>") {
		t.Errorf("POST /fleet/alerting (rejected): posted route name rendered UNESCAPED (XSS)\nbody:\n%s", body)
	}
}

// TestFleetAlertingConflictReturns409KeepsInput pins core.ErrConflict's
// fixed 409 message and input preservation.
func TestFleetAlertingConflictReturns409KeepsInput(t *testing.T) {
	fleet := &fakeFleet{setAlertingErr: core.ErrConflict}
	d := fleetAdminDeps(t, fleet)
	form := fleetAlertingSaveForm("1") // stale version
	rr := fleetAdminPost(t, d, "/fleet/alerting", form)
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST /fleet/alerting (conflict) status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "the alerting config changed since you loaded it") {
		t.Errorf("POST /fleet/alerting (conflict): missing the fixed reload message\nbody:\n%s", body)
	}
	if !strings.Contains(body, "r1") {
		t.Errorf("POST /fleet/alerting (conflict): posted route not preserved\nbody:\n%s", body)
	}
}

// TestFleetAlertingRouteTesterMultiplePolicies pins the Continue fan-out
// case: RouteTest returning several policies renders every one of them
// (name, steps, repeat) plus any suppression -- viewer-accessible, no CSRF.
func TestFleetAlertingRouteTesterMultiplePolicies(t *testing.T) {
	sendResolved := true
	fleet := &fakeFleet{routeTestResult: core.RouteDecision{
		Route: "r1",
		Policies: []core.Policy{
			{Name: "immediate", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"telegram"}}}, SendResolved: &sendResolved},
			{Name: "escalation", Steps: []core.PolicyStep{{After: "5m", Channels: []string{"email", "sms"}}}, RepeatEvery: "1h", SendResolved: &sendResolved},
		},
		Suppressed: "maintenance nightly",
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetAlertingViewerPost(t, d, "/fleet/alerting/test", url.Values{"node": {"web1"}, "rule": {"disk_pct"}, "severity": {"critical"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting/test status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.lastRouteTest.Node != "web1" || fleet.lastRouteTest.Rule != "disk_pct" || fleet.lastRouteTest.Severity != "critical" {
		t.Errorf("RouteTest called with %+v, want node=web1 rule=disk_pct severity=critical", fleet.lastRouteTest)
	}
	body := rr.Body.String()
	for _, want := range []string{"immediate", "escalation", "telegram", "email", "sms", "5m", "1h", "maintenance nightly"} {
		if !strings.Contains(body, want) {
			t.Errorf("POST /fleet/alerting/test: missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestFleetRulesStateFragmentNoDataAndFiring pins the /fleet/rules/state
// fragment's two key states: a rule with no value yet renders "no data",
// and a firing rule renders "firing" as literal text (never color-only).
func TestFleetRulesStateFragmentNoDataAndFiring(t *testing.T) {
	fleet := &fakeFleet{ruleStates: []core.RuleState{
		{Name: "quiet", Expr: "avg(cpu_pct) > 90", NoData: true},
		{Name: "loud", Expr: "avg(mem_pct) > 80", HasValue: true, Value: 92.5, Firing: true, Since: 1000},
	}}
	d := fleetAdminDeps(t, fleet)
	rr := fleetGetAsViewer(t, d, "/fleet/rules/state")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /fleet/rules/state status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"quiet", "no data", "loud", "92.5", "firing"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /fleet/rules/state: missing %q\nbody:\n%s", want, body)
		}
	}
}
