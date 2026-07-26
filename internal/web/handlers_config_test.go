//go:build web

package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"serverwatch/internal/config"
)

// configTestDeps builds a Deps whose Cfg/Reload behave like the real
// daemon's (see internal/serverwatch/daemon.go's reload closure): Cfg()
// returns whatever was last successfully Reload()ed, so a test can POST
// /config and then assert against Cfg() the same way the real web server
// would after a live SIGHUP-free reload. reloadErr, if set, makes Reload
// fail without mutating the stored config — for the "Reload itself fails"
// path.
func configTestDeps(t *testing.T) (d Deps, cfg **config.Config, reloadCalled *bool) {
	t.Helper()
	d = enrollTestDeps(t)
	c := config.Default()
	cfgPtr := &c
	called := false
	d.Cfg = func() *config.Config { return *cfgPtr }
	d.Reload = func(nc *config.Config) error {
		called = true
		*cfgPtr = nc
		return nil
	}
	return d, cfgPtr, &called
}

// TestConfigSaveRejectsBadValueWithNoWrite pins the core TDD obligation: an
// invalid field value rejects with 400, Reload is never called, and the
// live config is completely untouched.
func TestConfigSaveRejectsBadValueWithNoWrite(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	before := *(*cfg)
	form := baseConfigForm()
	form.Set("disk_pct", "not-a-number")
	rr := postForm(h, "/config", form, cookie, csrf)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite invalid input")
	}
	if (*cfg).Thresholds.DiskPct != before.Thresholds.DiskPct {
		t.Errorf("DiskPct = %v, want unchanged %v", (*cfg).Thresholds.DiskPct, before.Thresholds.DiskPct)
	}
}

// baseConfigForm returns a fully-populated, valid /config POST body mirroring
// what the rendered form always submits (every field, since browsers always
// submit text/number/time inputs and hidden fields regardless of whether the
// user touched them).
func baseConfigForm() url.Values {
	return url.Values{
		"disk_pct":      {"92"},
		"mem_pct":       {"85"},
		"temp_c":        {"82"},
		"anomaly_sigma": {"3.5"},
		"quiet_from":    {"23"},
		"quiet_to":      {"8"},
		"daily_time":    {"09:00"},
		"weekly_day":    {"mon"},
		"weekly_time":   {"09:00"},
		"deadman_url":   {""},
	}
}

// TestConfigSavePersistsReloadsAndAudits pins the success path: a valid POST
// persists (Reload called with the new values), and an audit record is
// written for each field that actually changed.
func TestConfigSavePersistsReloadsAndAudits(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := baseConfigForm()
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !*reloadCalled {
		t.Fatal("Reload was not called")
	}
	if (*cfg).Thresholds.DiskPct != 92 {
		t.Errorf("DiskPct = %v, want 92", (*cfg).Thresholds.DiskPct)
	}
	if (*cfg).Thresholds.MemPct != 85 {
		t.Errorf("MemPct = %v, want 85", (*cfg).Thresholds.MemPct)
	}

	recs := readAuditRecords(t, d.StateDir)
	if len(recs) == 0 {
		t.Fatal("no audit records written")
	}
	found := false
	for _, r := range recs {
		if r.Action == "config.set" && r.Key == "thresholds.disk_pct" {
			found = true
			if r.New != "92" {
				t.Errorf("audit new = %q, want 92", r.New)
			}
			if r.User != "root" {
				t.Errorf("audit user = %q, want root", r.User)
			}
		}
	}
	if !found {
		t.Errorf("no audit record for thresholds.disk_pct change, got: %+v", recs)
	}
}

// TestConfigSaveUnchangedFieldsWriteNoAudit pins that re-submitting the same
// values (no actual change) does not spam the audit log.
func TestConfigSaveUnchangedFieldsWriteNoAudit(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	// First POST establishes values matching config.Default() exactly.
	form := url.Values{
		"disk_pct":      {"90"},
		"mem_pct":       {"90"},
		"temp_c":        {"80"},
		"anomaly_sigma": {"3"},
		"quiet_from":    {"23"},
		"quiet_to":      {"8"},
		"daily_time":    {"09:00"},
		"weekly_day":    {"mon"},
		"weekly_time":   {"09:00"},
		"deadman_url":   {""},
	}
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	recs := readAuditRecords(t, d.StateDir)
	if len(recs) != 0 {
		t.Fatalf("first POST (identical to defaults) wrote %d audit records, want 0: %+v", len(recs), recs)
	}
}

// TestConfigTargetEditsRoundTrip pins the monitors table: posting a target's
// enabled flag and threshold round-trips into cfg.Targets, and clearing the
// threshold field removes the override.
func TestConfigTargetEditsRoundTrip(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := baseConfigForm()
	form.Add("target_name", "disk:/")
	form.Add("target_threshold", "95")
	form.Add("target_enabled", "disk:/") // checked
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !(*cfg).TargetEnabled("disk:/") {
		t.Error("disk:/ should be enabled")
	}
	thr, ok := (*cfg).TargetThreshold("disk:/")
	if !ok || thr != 95 {
		t.Errorf("threshold = %v, %v, want 95, true", thr, ok)
	}

	// Second POST: box unchecked (target_enabled omitted entirely, matching
	// how browsers submit unchecked checkboxes), threshold cleared.
	form2 := baseConfigForm()
	form2.Add("target_name", "disk:/")
	form2.Add("target_threshold", "")
	rr2 := postForm(h, "/config", form2, cookie, csrf)
	if rr2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	if (*cfg).TargetEnabled("disk:/") {
		t.Error("disk:/ should now be disabled")
	}
	if _, ok := (*cfg).TargetThreshold("disk:/"); ok {
		t.Error("threshold override should have been cleared")
	}
}

// TestConfigTargetBadThresholdRejectedWithNoWrite pins that an unparseable
// per-target threshold rejects with 400 and leaves Targets untouched, same
// "no write on bad value" contract as the scalar fields.
func TestConfigTargetBadThresholdRejectedWithNoWrite(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := baseConfigForm()
	form.Add("target_name", "disk:/")
	form.Add("target_threshold", "not-a-number")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite an invalid target threshold")
	}
	if _, ok := (*cfg).TargetThreshold("disk:/"); ok {
		t.Error("threshold override should not have been written")
	}
}

// TestConfigPageRendersFormAndCurrentValues pins GET /config: it must render
// through the app shell with the current thresholds/monitors filled in.
func TestConfigPageRendersFormAndCurrentValues(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.Snapshot = func() DashboardView {
		return DashboardView{Disks: []DiskView{{Mount: "/", UsagePct: 91}}}
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`class="side"`,
		`name="disk_pct" value="90"`,
		`disk:/`,
		`action="/config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config page missing %q:\n%s", want, body)
		}
	}
}

// TestConfigRoutesAreAdminGated pins RBAC: viewer gets 403, anon redirects to
// /login, for both GET and POST /config.
func TestConfigRoutesAreAdminGated(t *testing.T) {
	d, _, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	viewerReq := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/config")
	viewerRR := httptest.NewRecorder()
	h.ServeHTTP(viewerRR, viewerReq)
	if viewerRR.Code != http.StatusForbidden {
		t.Errorf("GET /config as viewer status = %d, want 403", viewerRR.Code)
	}

	anonRR := httptest.NewRecorder()
	h.ServeHTTP(anonRR, httptest.NewRequest(http.MethodGet, "/config", nil))
	if anonRR.Code != http.StatusFound {
		t.Errorf("GET /config anon status = %d, want 302", anonRR.Code)
	}

	postRR := httptest.NewRecorder()
	h.ServeHTTP(postRR, httptest.NewRequest(http.MethodPost, "/config", strings.NewReader("disk_pct=90")))
	if postRR.Code != http.StatusFound {
		t.Errorf("POST /config anon status = %d, want 302 (redirect to /login)", postRR.Code)
	}
}

// TestConfigSaveRequiresCSRF pins that a signed-in admin POST without a valid
// CSRF token is rejected, matching every other mutation route.
func TestConfigSaveRequiresCSRF(t *testing.T) {
	d, _, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, _ := seedAdmin(t, "root", users, sessions)

	rr := postForm(h, "/config", baseConfigForm(), cookie, "" /* no CSRF token */)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite missing CSRF token")
	}
}
