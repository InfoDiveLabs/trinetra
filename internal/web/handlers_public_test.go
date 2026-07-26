//go:build web

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// publicTestSnapshot is a DashboardView with a distinct, individually
// greppable value on every field the public-panel catalog might render, so
// tests can assert a value string's presence/absence unambiguously.
func publicTestSnapshot() DashboardView {
	return DashboardView{
		Online:            true,
		CPU:               42,
		MemPct:            77,
		SwapPct:           13,
		Load1:             1.5,
		TempC:             61,
		UnitsFailed:       1,
		UnitsTotal:        6,
		ContainersRunning: 3,
		ContainersTotal:   4,
		Disks:             []DiskView{{Mount: "/", UsagePct: 55}, {Mount: "/data", UsagePct: 88}},
		NetRxBps:          2048,
		NetTxBps:          1024,
	}
}

// TestPublicPageDisabledReturns404 pins the headline security requirement:
// public.enabled=false must 404 rather than reveal the page exists at all
// (not render an empty/disabled state, not redirect to login).
func TestPublicPageDisabledReturns404(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = false
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /public with public.enabled=false status = %d, want 404", rr.Code)
	}
}

// TestPublicPageRendersOnlyAllowlistedPanels is the core security-critical
// obligation: with public.enabled=true and public.panels=["cpu"], only cpu's
// value appears in the rendered HTML. mem/swap/disk values are present in
// the SAME snapshot but must never appear on the page, proving the allowlist
// is enforced server-side (by iterating public.panels, not by rendering the
// full dashboard and hiding the rest).
func TestPublicPageRendersOnlyAllowlistedPanels(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /public status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "42%") {
		t.Errorf("public page missing the allowlisted cpu value 42%%:\n%s", body)
	}
	// mem (77%), swap (13%), and both disk mounts (55%/88%) are all present
	// in the snapshot but NOT in public.panels — none of their values may
	// leak onto the anonymous page.
	for _, leaked := range []string{"77%", "13%", "55%", "88%"} {
		if strings.Contains(body, leaked) {
			t.Errorf("public page leaked non-allowlisted value %q:\n%s", leaked, body)
		}
	}
}

// TestPublicPageIgnoresQueryStringPanelOverride pins that an attacker can't
// smuggle a non-curated metric onto the page via a query parameter (or any
// other request input) — the handler only ever consults cfg.Public.Panels,
// never anything caller-supplied.
func TestPublicPageIgnoresQueryStringPanelOverride(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public?panel=mem&panels=mem,swap", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "77%") {
		t.Errorf("query-string override leaked mem's value onto /public:\n%s", rr.Body.String())
	}
}

// TestPublicPageNoAuthLeakage pins the broader leakage requirements: no
// session cookie is ever set on GET /public, and the body carries no
// control affordances (forms/buttons) or links into authed/admin routes —
// only the curated tiles.
func TestPublicPageNoAuthLeakage(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu", "mem"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("GET /public set cookies: %+v, want none", cookies)
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"<form", "<button", `href="/login"`, `href="/config"`, `href="/users"`, `href="/channels"`, `href="/settings`, "csrf-token", `class="side"`, `class="topbar"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("public page leaked control/admin affordance %q:\n%s", forbidden, body)
		}
	}
}

// TestPublicPageEmptyAllowlistRendersNoPanels pins the degenerate case:
// enabled=true with an empty (or all-unavailable) allowlist renders 200 with
// no tiles rather than erroring or falling back to some default set.
func TestPublicPageEmptyAllowlistRendersNoPanels(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = nil
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "42%") {
		t.Errorf("empty allowlist still rendered a panel value:\n%s", rr.Body.String())
	}
}

// TestPublicPageOmitsUnavailableMetric pins that an allowlisted panel whose
// underlying data isn't currently available (e.g. a disk mount that no
// longer exists, or a zero thermal sensor reading) is simply omitted, never
// rendered blank/zero.
func TestPublicPageOmitsUnavailableMetric(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu", "disk:/missing", "temp"}
	snap := publicTestSnapshot()
	snap.TempC = 0
	d.Snapshot = func() DashboardView { return snap }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "42%") {
		t.Errorf("cpu missing even though available:\n%s", body)
	}
	if strings.Contains(body, "Temp") || strings.Contains(body, "/missing") {
		t.Errorf("unavailable metrics rendered:\n%s", body)
	}
}

// TestPublicSettingsPageRendersPickerForAdmin pins GET /settings/public: it
// lists the panel catalog with checkboxes reflecting the current
// public.panels selection.
func TestPublicSettingsPageRendersPickerForAdmin(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/settings/public")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="panel" value="cpu" checked`) {
		t.Errorf("picker missing checked cpu checkbox:\n%s", body)
	}
	if !strings.Contains(body, `name="panel" value="mem"`) || strings.Contains(body, `name="panel" value="mem" checked`) {
		t.Errorf("picker missing unchecked mem checkbox:\n%s", body)
	}
	if !strings.Contains(body, `name="panel" value="disk:/"`) {
		t.Errorf("picker missing a live disk mount checkbox:\n%s", body)
	}
}

// TestPublicSettingsRoutesAreAdminGated pins RBAC on /settings/public,
// mirroring TestChannelsRoutesAreAdminGated (rbac_test.go already exercises
// this route inside TestRequireRoleAppliesAcrossAllAdminRoutes/
// TestRequireRoleAnonRedirectsAcrossAllAdminRoutes too — this test pins it
// directly for this file's own regression coverage).
func TestPublicSettingsRoutesAreAdminGated(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/settings/public")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusForbidden {
		t.Errorf("GET /settings/public as viewer status = %d, want 403", viewerRR.Code)
	}

	anonRR := httptest.NewRecorder()
	h.ServeHTTP(anonRR, httptest.NewRequest(http.MethodGet, "/settings/public", nil))
	if anonRR.Code != http.StatusFound {
		t.Errorf("GET /settings/public anon status = %d, want 302", anonRR.Code)
	}
}

// TestPublicSettingsSaveRoundTripsAndAudits pins POST /settings/public: the
// posted enabled flag + checked panel ids persist into cfg.Public, Reload is
// called, and an audit record captures the change.
func TestPublicSettingsSaveRoundTripsAndAudits(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{
		"enabled": {"1"},
		"panel":   {"cpu", "mem", "disk:/"},
	}
	rr := postForm(h, "/settings/public", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !*reloadCalled {
		t.Fatal("Reload was not called")
	}
	if !(*cfg).Public.Enabled {
		t.Error("public.enabled was not persisted as true")
	}
	got := map[string]bool{}
	for _, p := range (*cfg).Public.Panels {
		got[p] = true
	}
	for _, want := range []string{"cpu", "mem", "disk:/"} {
		if !got[want] {
			t.Errorf("public.panels = %+v, want to contain %q", (*cfg).Public.Panels, want)
		}
	}

	recs := readAuditRecords(t, d.StateDir)
	foundEnabled, foundPanels := false, false
	for _, r := range recs {
		if r.Action == "public.set" && r.Key == "public.enabled" {
			foundEnabled = true
		}
		if r.Action == "public.set" && r.Key == "public.panels" {
			foundPanels = true
		}
	}
	if !foundEnabled || !foundPanels {
		t.Errorf("missing public.set audit records, got: %+v", recs)
	}
}

// TestPublicSettingsSaveUncheckingAllPanelsClearsThem pins that omitting
// every "panel" checkbox (all unchecked) clears public.panels to empty
// rather than leaving stale entries — an admin explicitly hiding everything
// must actually hide everything.
func TestPublicSettingsSaveUncheckingAllPanelsClearsThem(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Panels = []string{"cpu", "mem"}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/settings/public", url.Values{}, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if len((*cfg).Public.Panels) != 0 {
		t.Errorf("public.panels = %+v, want empty after unchecking all", (*cfg).Public.Panels)
	}
	if (*cfg).Public.Enabled {
		t.Error("public.enabled should be false after posting without the enabled checkbox")
	}
}

// TestPublicSettingsSaveRejectsUnknownPanelWithNoWrite pins that the
// config-layer allowlist (validatePublicPanel) is still enforced even if a
// forged POST includes a "panel" value the rendered form never offers —
// defense in depth against a tampered request, not just a trusted browser.
func TestPublicSettingsSaveRejectsUnknownPanelWithNoWrite(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/settings/public", url.Values{"panel": {"cpu", "users"}}, cookie, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite an invalid panel id")
	}
	if len((*cfg).Public.Panels) != 0 {
		t.Errorf("public.panels = %+v, want unchanged/empty", (*cfg).Public.Panels)
	}
}

// TestPublicSettingsSaveRequiresCSRF mirrors TestChannelsMutationsRequireCSRF
// for /settings/public.
func TestPublicSettingsSaveRequiresCSRF(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/settings/public", url.Values{"panel": {"cpu"}}, cookie, "" /* no CSRF */)
	if rr.Code != http.StatusForbidden {
		t.Errorf("POST /settings/public without CSRF status = %d, want 403", rr.Code)
	}
}
