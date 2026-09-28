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

// seedViewer puts a RoleViewer *User in store and mints a live session for
// it, returning the session cookie plus that session's CSRF token --
// mirrors seedAdmin (handlers_users_test.go) for the viewer role, needed
// now that POST /fleet/alerting/test requires CSRF too (fix round 1
// IMPORTANT 1).
func seedViewer(t *testing.T, name string, users UserStore, sessions SessionStore) (*User, *http.Cookie, string) {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: name, Role: RoleViewer, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed viewer Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	return u, &http.Cookie{Name: sessionCookieName, Value: sess.ID}, sess.CSRF
}

// fleetAlertingViewerPost issues a viewer-signed-in, CSRF-valid, form-
// encoded POST against d's handler -- for POST /fleet/alerting/test, which
// stays viewer-gated but still requires a valid CSRF token like any other
// signed-in POST.
func fleetAlertingViewerPost(t *testing.T, d Deps, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedViewer(t, "viewer-user", users, sessions)
	return postForm(h, target, form, cookie, csrf)
}

// fleetAdminSessionGet issues an admin-signed-in GET against d's handler and
// returns the response body, the handler, and the session cookie used --
// so a follow-up POST (formValuesForButton + postForm) can act as the SAME
// session: the csrf_token extracted from the rendered page belongs to this
// one session, so the follow-up POST must reuse its cookie, not a fresh one.
func fleetAdminSessionGet(t *testing.T, d Deps, target string) (body string, h http.Handler, cookie *http.Cookie) {
	t.Helper()
	h = newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ = seedAdmin(t, "root", users, sessions)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200, body: %s", target, rr.Code, rr.Body.String())
	}
	return rr.Body.String(), h, cookie
}

// fleetAlertingFixtureCfg is this file's shared "pre-existing config"
// fixture: one route, two policies, and one aggregate rule -- fix round 1's
// own choice of shape for the browser-faithful tests (a route-only op or
// edit must never disturb the two policies or the rule).
func fleetAlertingFixtureCfg() core.AlertingConfig {
	return core.AlertingConfig{
		Version: 5,
		Routes:  []core.Route{{Name: "r0", Matchers: []core.Matcher{{Tag: "web"}}, Policy: "p1"}},
		Policies: []core.Policy{
			{Name: "p1", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}},
			{Name: "p2", Steps: []core.PolicyStep{{After: "5m", Channels: []string{"email"}}}, RepeatEvery: "1h"},
		},
		Rules:         []core.AggregateRule{{Name: "rule1", Expr: "avg(cpu_pct) > 90", Severity: "critical"}},
		DefaultPolicy: "p1",
	}
}

// fleetAlertingSaveForm returns a minimal, individually valid structured-
// form POST body for one route (tag=web -> policy p1) and one policy (p1,
// one immediate step to every channel), op=save -- used only by the tests
// below that don't need a real rendered page (a bare RBAC/size check), not
// as a stand-in for what the template actually renders.
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

// ---------------------------------------------------------------------------
// Page/RBAC basics
// ---------------------------------------------------------------------------

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

// TestFleetAlertingTestMissingCSRFForbidden pins fix round 1 IMPORTANT 1:
// POST /fleet/alerting/test now requires CSRF too (the /channels/{name}/test
// precedent), even though a viewer may call it and it never mutates the
// saved config.
func TestFleetAlertingTestMissingCSRFForbidden(t *testing.T) {
	d := fleetAdminDeps(t, &fakeFleet{})
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedViewer(t, "viewer-user", users, sessions)
	rr := postForm(h, "/fleet/alerting/test", url.Values{"node": {"web1"}, "rule": {"disk_pct"}, "severity": {"critical"}}, cookie, "")
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST /fleet/alerting/test with no CSRF token = %d, want 403", rr.Code)
	}
}

// ---------------------------------------------------------------------------
// Browser-faithful structured-editor tests (fix round 1 CRITICAL): every
// POST here is built by rendering the real page, then extracting exactly
// the fields the form containing a given submit button would submit
// (formValuesForButton, formhelpers_test.go) -- never a hand-built
// url.Values fixture standing in for what the template "should" produce.
// This is what actually catches the round-1 bug: three separate <form>s
// (one per section) each fabricating blank rows for the other two sections
// on submit.
// ---------------------------------------------------------------------------

// TestFleetAlertingAddRoutePreservesPoliciesAndRules pins the CRITICAL fix:
// starting from a config with 2 policies and 1 rule, clicking "Add route"
// (a route-only op) must not wipe the policies or the rule from the
// re-rendered draft, and must not call SetAlerting at all.
func TestFleetAlertingAddRoutePreservesPoliciesAndRules(t *testing.T) {
	fleet := &fakeFleet{alertingCfg: fleetAlertingFixtureCfg()}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	values := formValuesForButton(t, body, "op", "add_route")
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=add_route status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 0 {
		t.Errorf("SetAlerting calls = %d, want 0 (add_route must not save)", fleet.setAlertingCalls)
	}
	got := rr.Body.String()
	for _, want := range []string{"p1", "p2", "rule1", ">Route 0<", ">Route 1<"} {
		if !strings.Contains(got, want) {
			t.Errorf("POST /fleet/alerting op=add_route: missing %q (policies/rule must survive a route-only op)\nbody:\n%s", want, got)
		}
	}
	if strings.Contains(got, "No policies configured") {
		t.Errorf("POST /fleet/alerting op=add_route: the 2 existing policies were wiped\nbody:\n%s", got)
	}
	// Both the editable "Aggregate rules" table's empty state and the
	// separate, unrelated live "Rules" (RuleStates) panel's empty state
	// happen to share the literal text "no aggregate rules configured" --
	// this fake has no RuleStates fixture at all, so the LIVE panel is
	// legitimately empty regardless of the editable draft. rule1's own
	// name appearing (asserted above) is what actually pins "the rule
	// survived the route-only op".
}

// TestFleetAlertingSaveSendsFullConfigUnchangedExceptRouteEdit pins the
// other half of the CRITICAL fix: clicking "Save" after editing only the
// route's name must submit the policies and rule UNCHANGED alongside that
// one route edit -- never fabricated/blank copies.
func TestFleetAlertingSaveSendsFullConfigUnchangedExceptRouteEdit(t *testing.T) {
	fleet := &fakeFleet{alertingCfg: fleetAlertingFixtureCfg()}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("route_0_name", "r0-renamed") // the only edit a user made
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=save status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 1 {
		t.Fatalf("SetAlerting calls = %d, want 1", fleet.setAlertingCalls)
	}

	got := fleet.setAlertingCfg
	if len(got.Routes) != 1 || got.Routes[0].Name != "r0-renamed" || got.Routes[0].Policy != "p1" {
		t.Errorf("SetAlerting cfg.Routes = %+v, want the one route renamed to r0-renamed", got.Routes)
	}

	want := fleetAlertingFixtureCfg()
	if len(got.Policies) != len(want.Policies) {
		t.Fatalf("SetAlerting cfg.Policies = %+v, want %d policies unchanged", got.Policies, len(want.Policies))
	}
	for i, wp := range want.Policies {
		gp := got.Policies[i]
		if gp.Name != wp.Name || len(gp.Steps) != len(wp.Steps) || gp.RepeatEvery != wp.RepeatEvery {
			t.Errorf("SetAlerting cfg.Policies[%d] = %+v, want unchanged %+v", i, gp, wp)
		}
	}
	if len(got.Rules) != 1 || got.Rules[0].Name != "rule1" || got.Rules[0].Expr != "avg(cpu_pct) > 90" || got.Rules[0].Severity != "critical" {
		t.Errorf("SetAlerting cfg.Rules = %+v, want rule1 unchanged", got.Rules)
	}
	if got.DefaultPolicy != "p1" {
		t.Errorf("SetAlerting cfg.DefaultPolicy = %q, want p1 unchanged", got.DefaultPolicy)
	}
}

// TestFleetAlertingFormSaveRoundTrip drives the WHOLE structured-editor flow
// through real rendered pages: starting from nothing, click "Add route",
// then "Add policy" (both against the SAME session/form, each response
// re-parsed for the next click), fill in the new route/policy fields, then
// click "Save" -- pinning both that add ops never fabricate/lose sections
// and that a real save round-trips end to end (actor, audit, converted
// config).
func TestFleetAlertingFormSaveRoundTrip(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	values := formValuesForButton(t, body, "op", "add_route")
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=add_route status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()

	values = formValuesForButton(t, body, "op", "add_policy")
	rr = postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=add_policy status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()

	values = formValuesForButton(t, body, "op", "save")
	values.Set("route_0_name", "r1")
	values.Set("route_0_policy", "p1")
	values.Set("route_0_matcher_0_tag", "web")
	values.Set("policy_0_name", "p1")
	values.Set("policy_0_step_0_after", "0s")
	values.Add("policy_0_step_0_channels", "*")
	values.Set("default_policy", "p1")
	rr = postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting op=save status = %d, want 200, body: %s", rr.Code, rr.Body.String())
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

	final := rr.Body.String()
	if !strings.Contains(final, "alerting config saved") {
		t.Errorf("POST /fleet/alerting: missing saved flash\nbody:\n%s", final)
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

// TestFleetAlertingValidationErrorKeepsInputInline pins global-
// constraints.md's ruling: a rejected save re-renders at 400 with the
// backend's message inline next to the field it names (a route, here), and
// every posted value preserved -- driven off the real rendered "Save"
// button's form fields, not a hand-built fixture.
func TestFleetAlertingValidationErrorKeepsInputInline(t *testing.T) {
	fleet := &fakeFleet{alertingCfg: fleetAlertingFixtureCfg()}
	fleet.setAlertingErr = fmt.Errorf(`route "r0": unknown policy "p1"`)
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	values := formValuesForButton(t, body, "op", "save")
	values.Set("route_0_name", "weird<name>")
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/alerting (rejected) status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if !strings.Contains(html.UnescapeString(got), `unknown policy "p1"`) {
		t.Errorf("POST /fleet/alerting (rejected): missing backend error text\nbody:\n%s", got)
	}
	if !strings.Contains(got, "weird&lt;name&gt;") {
		t.Errorf("POST /fleet/alerting (rejected): posted route name not preserved/escaped\nbody:\n%s", got)
	}
	if strings.Contains(got, "weird<name>") {
		t.Errorf("POST /fleet/alerting (rejected): posted route name rendered UNESCAPED (XSS)\nbody:\n%s", got)
	}
	// The rejection must not have also fabricated blank policies/rules.
	if !strings.Contains(got, "p2") || !strings.Contains(got, "rule1") {
		t.Errorf("POST /fleet/alerting (rejected): policies/rule not preserved\nbody:\n%s", got)
	}
}

// TestFleetAlertingConflictReturns409KeepsInput pins core.ErrConflict's
// fixed 409 message and input preservation, again driven off the real
// rendered "Save" button's form fields.
func TestFleetAlertingConflictReturns409KeepsInput(t *testing.T) {
	fleet := &fakeFleet{alertingCfg: fleetAlertingFixtureCfg()}
	fleet.setAlertingErr = core.ErrConflict
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	values := formValuesForButton(t, body, "op", "save")
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST /fleet/alerting (conflict) status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
	got := rr.Body.String()
	if !strings.Contains(got, "the alerting config changed since you loaded it") {
		t.Errorf("POST /fleet/alerting (conflict): missing the fixed reload message\nbody:\n%s", got)
	}
	if !strings.Contains(got, "r0") || !strings.Contains(got, "p2") || !strings.Contains(got, "rule1") {
		t.Errorf("POST /fleet/alerting (conflict): posted config not fully preserved\nbody:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Edit as JSON
// ---------------------------------------------------------------------------

// TestFleetAlertingJSONSave pins the "edit as JSON" mode end to end,
// clicking the real rendered "Save JSON" button (proving its CSRF token is
// actually wired up, not just assumed).
func TestFleetAlertingJSONSave(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	body, h, cookie := fleetAdminSessionGet(t, d, "/fleet/alerting")

	raw := `{"version":0,"routes":[{"name":"jr","matchers":[{"tag":"web"}],"policy":"jp"}],"policies":[{"name":"jp","steps":[{"after":"0s","channels":["*"]}]}],"default_policy":"jp"}`
	values := formValuesForButton(t, body, "save", "json")
	values.Set("json_config", raw)
	rr := postForm(h, "/fleet/alerting", values, cookie, "")
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

// TestFleetAlertingJSONUnknownFieldRejectedInline pins fix round 1
// IMPORTANT 2: a typo'd field ("send_resolve" instead of "send_resolved")
// is rejected by DisallowUnknownFields, naming the field, WITHOUT ever
// calling SetAlerting, and the textarea keeps exactly what was posted.
func TestFleetAlertingJSONUnknownFieldRejectedInline(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	raw := `{"version":0,"policies":[{"name":"p1","steps":[{"after":"0s","channels":["*"]}],"send_resolve":true}],"default_policy":"p1"}`
	rr := fleetAdminPost(t, d, "/fleet/alerting", url.Values{"mode": {"json"}, "json_config": {raw}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/alerting mode=json (unknown field) status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 0 {
		t.Errorf("SetAlerting calls = %d, want 0 (an unknown field must be rejected before SetAlerting is ever called)", fleet.setAlertingCalls)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "send_resolve") {
		t.Errorf("POST /fleet/alerting mode=json (unknown field): error doesn't name the field\nbody:\n%s", body)
	}
	if !strings.Contains(body, "default_policy") {
		t.Errorf("POST /fleet/alerting mode=json (unknown field): textarea did not preserve the posted JSON\nbody:\n%s", body)
	}
}

// TestFleetAlertingJSONTrailingDataRejected pins the other half of
// IMPORTANT 2: the body must contain EXACTLY one JSON value.
func TestFleetAlertingJSONTrailingDataRejected(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	raw := `{"version":0,"default_policy":"p1"} {"extra":true}`
	rr := fleetAdminPost(t, d, "/fleet/alerting", url.Values{"mode": {"json"}, "json_config": {raw}})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("POST /fleet/alerting mode=json (trailing data) status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.setAlertingCalls != 0 {
		t.Errorf("SetAlerting calls = %d, want 0", fleet.setAlertingCalls)
	}
}

// TestFleetAlertingRequestTooLarge pins the MINOR fix: a body over
// alertingMaxBodyBytes (256 KiB) is rejected at 413 with an inline message,
// before ever being parsed into a draft.
func TestFleetAlertingRequestTooLarge(t *testing.T) {
	fleet := &fakeFleet{}
	d := fleetAdminDeps(t, fleet)
	huge := strings.Repeat("a", 300*1024)
	rr := fleetAdminPost(t, d, "/fleet/alerting", url.Values{"mode": {"json"}, "json_config": {huge}})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /fleet/alerting (oversized body) status = %d, want 413, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "too large") {
		t.Errorf("POST /fleet/alerting (oversized body): missing inline message\nbody:\n%s", rr.Body.String())
	}
	if fleet.setAlertingCalls != 0 {
		t.Errorf("SetAlerting calls = %d, want 0", fleet.setAlertingCalls)
	}
}

// ---------------------------------------------------------------------------
// Route tester
// ---------------------------------------------------------------------------

// TestFleetAlertingRouteTesterMultiplePolicies pins the Continue fan-out
// case: RouteTest returning several policies renders every one of them
// (name, steps, repeat) plus any suppression -- viewer-accessible, CSRF
// required.
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

// TestFleetAlertingRouteTesterFormSubmitsRealCSRF drives the tester through
// the real rendered form (formValuesForButton), pinning that its own
// csrf_token hidden field is actually present and valid, not merely assumed.
func TestFleetAlertingRouteTesterFormSubmitsRealCSRF(t *testing.T) {
	fleet := &fakeFleet{routeTestResult: core.RouteDecision{Route: "r1", Policies: []core.Policy{{Name: "p1", Steps: []core.PolicyStep{{After: "0s", Channels: []string{"*"}}}}}}}
	d := fleetAdminDeps(t, fleet)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedViewer(t, "viewer-user", users, sessions)
	getReq := httptest.NewRequest(http.MethodGet, "/fleet/alerting", nil)
	getReq.AddCookie(cookie)
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET /fleet/alerting as viewer status = %d, want 200", getRR.Code)
	}

	values := formValuesForButton(t, getRR.Body.String(), "test", "route")
	values.Set("rule", "disk_pct")
	rr := postForm(h, "/fleet/alerting/test", values, cookie, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /fleet/alerting/test (real form) status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if fleet.lastRouteTest.Rule != "disk_pct" {
		t.Errorf("RouteTest called with %+v, want rule=disk_pct", fleet.lastRouteTest)
	}
}

// ---------------------------------------------------------------------------
// Rules panel
// ---------------------------------------------------------------------------

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
