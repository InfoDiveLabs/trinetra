package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// configTestDeps builds a Deps whose Cfg/Reload/API.ApplyConfig behave like
// the real daemon's (see internal/trinetra/daemon.go's reload closure):
// Cfg() returns whatever was last successfully applied, so a test can POST
// /config or /channels and then assert against Cfg() the same way the real
// web server would after a live SIGHUP-free reload. Both Deps.Reload (still
// read directly by the /public settings save, handlers_public.go) and
// Deps.API.ApplyConfig (task 8: what configSaveHandler/the channels handlers
// now call instead) are wired to the SAME closure, so every existing test
// asserting against the returned cfg/reloadCalled keeps working no matter
// which of the two a given handler happens to call.
func configTestDeps(t *testing.T) (d Deps, cfg **config.Config, reloadCalled *bool) {
	t.Helper()
	d = enrollTestDeps(t)
	c := config.Default()
	cfgPtr := &c
	called := false
	d.Cfg = func() *config.Config { return *cfgPtr }
	apply := func(nc *config.Config) error {
		called = true
		*cfgPtr = nc
		return nil
	}
	d.Reload = apply
	d.API = fakeAPI{applyConfig: apply}
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
// user touched them). Checkbox fields that default to true (critical_
// overrides_quiet, collect.*) are included checked so a bare baseConfigForm()
// post doesn't flip them off as a side effect; baseline_alerts (defaults to
// false) is deliberately omitted, matching an unchecked box.
func baseConfigForm() url.Values {
	return url.Values{
		"disk_pct":                 {"92"},
		"mem_pct":                  {"85"},
		"temp_c":                   {"82"},
		"cpu_pct":                  {"93"},
		"swap_pct":                 {"55"},
		"anomaly_sigma":            {"3.5"},
		"baseline_min_pct":         {"0.2"},
		"quiet_from":               {"23"},
		"quiet_to":                 {"8"},
		"critical_overrides_quiet": {"1"},
		"fast_interval":            {"5"},
		"sample_interval":          {"60"},
		"heartbeat_interval":       {"45"},
		"collect_container_stats":  {"1"},
		"collect_net_throughput":   {"1"},
		"collect_services":         {"1"},
		"collect_processes":        {"1"},
		"collect_smart_attrs":      {"1"},
		"smart_interval":           {"900"},
		"raw_retention":            {"72h"},
		"rollup_retention":         {"900h"},
		"daily_time":               {"09:00"},
		"weekly_day":               {"mon"},
		"weekly_time":              {"09:00"},
		"deadman_url":              {""},
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

// TestConfigIdentityPanelRendersAndSaves pins the identity setup UI (#101/#102):
// the config page renders the server-name field + public-IP toggle, and a POST
// applies both onto the live config.
func TestConfigIdentityPanelRendersAndSaves(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	// GET renders the identity inputs.
	getReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	body := getRR.Body.String()
	for _, want := range []string{`name="server_name"`, `name="collect_public_ip"`} {
		if !strings.Contains(body, want) {
			t.Errorf("config page missing identity field %q", want)
		}
	}

	// POST sets a server name and enables the public-IP lookup.
	form := baseConfigForm()
	form.Set("server_name", "attic-pi")
	form.Set("collect_public_ip", "1")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if (*cfg).Name != "attic-pi" {
		t.Errorf("cfg.Name = %q, want attic-pi", (*cfg).Name)
	}
	if !(*cfg).PublicIPEnabled() {
		t.Error("collect.public_ip not enabled after POST")
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
		"disk_pct":                 {"90"},
		"mem_pct":                  {"90"},
		"temp_c":                   {"80"},
		"cpu_pct":                  {"95"},
		"swap_pct":                 {"50"},
		"anomaly_sigma":            {"3"},
		"baseline_min_pct":         {"0.15"},
		"quiet_from":               {"23"},
		"quiet_to":                 {"8"},
		"critical_overrides_quiet": {"1"}, // config.Default() has this true
		"fast_interval":            {"5"},
		"sample_interval":          {"60"},
		"heartbeat_interval":       {"30"},
		"collect_container_stats":  {"1"}, // unset defaults to true
		"collect_net_throughput":   {"1"},
		"collect_services":         {"1"},
		"collect_processes":        {"1"},
		"collect_smart_attrs":      {"1"},
		"smart_interval":           {"1800"},
		"raw_retention":            {"48h"},
		"rollup_retention":         {"720h"},
		"daily_time":               {"09:00"},
		"weekly_day":               {"mon"},
		"weekly_time":              {"09:00"},
		"deadman_url":              {""},
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

// TestConfigSaveBaselineAlertsToggleRoundTrips pins the bool-toggle contract
// for the important "turn anomaly alerting on/off" switch: on -> off -> on,
// each persisting via Reload and each transition audited. Off is submitted by
// omitting the field entirely, matching how a browser submits an unchecked
// checkbox (never a present-but-empty value).
func TestConfigSaveBaselineAlertsToggleRoundTrips(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	if (*cfg).BaselineAlerts {
		t.Fatal("test setup: BaselineAlerts should default to false")
	}

	// on
	form := baseConfigForm()
	form.Set("baseline_alerts", "1")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !(*cfg).BaselineAlerts {
		t.Fatal("BaselineAlerts should be true after enabling")
	}
	recs := readAuditRecords(t, d.StateDir)
	found := false
	for _, r := range recs {
		if r.Action == "config.set" && r.Key == "baseline_alerts" && r.Old == "false" && r.New == "true" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit record for baseline_alerts false->true, got: %+v", recs)
	}

	// off (checkbox omitted entirely)
	form2 := baseConfigForm()
	rr2 := postForm(h, "/config", form2, cookie, csrf)
	if rr2.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200, body: %s", rr2.Code, rr2.Body.String())
	}
	if (*cfg).BaselineAlerts {
		t.Fatal("BaselineAlerts should be false after disabling (box left unchecked)")
	}
	recs = readAuditRecords(t, d.StateDir)
	found = false
	for _, r := range recs {
		if r.Action == "config.set" && r.Key == "baseline_alerts" && r.Old == "true" && r.New == "false" {
			found = true
		}
	}
	if !found {
		t.Errorf("no audit record for baseline_alerts true->false, got: %+v", recs)
	}

	// on again
	form3 := baseConfigForm()
	form3.Set("baseline_alerts", "1")
	rr3 := postForm(h, "/config", form3, cookie, csrf)
	if rr3.Code != http.StatusOK {
		t.Fatalf("re-enable status = %d, want 200, body: %s", rr3.Code, rr3.Body.String())
	}
	if !(*cfg).BaselineAlerts {
		t.Fatal("BaselineAlerts should be true after re-enabling")
	}
}

// TestConfigSaveIntervalsRoundTripAndRejectsBadCombo pins the int fields
// (fast/sample/heartbeat interval): a valid, consistent combo round-trips,
// and an inconsistent one (sample_interval not a multiple of fast_interval --
// config.Config.Set's existing validator) rejects with 400 and writes
// nothing, exactly like every other bad-value case.
func TestConfigSaveIntervalsRoundTripAndRejectsBadCombo(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := baseConfigForm()
	form.Set("fast_interval", "10")
	form.Set("sample_interval", "120")
	form.Set("heartbeat_interval", "20")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if (*cfg).FastInterval != 10 || (*cfg).SampleInterval != 120 || (*cfg).HeartbeatInterval != 20 {
		t.Errorf("intervals = %d/%d/%d, want 10/120/20", (*cfg).FastInterval, (*cfg).SampleInterval, (*cfg).HeartbeatInterval)
	}

	// Bad combo: sample_interval (60) is not a multiple of fast_interval (7).
	before := *(*cfg)
	form2 := baseConfigForm()
	form2.Set("fast_interval", "7")
	form2.Set("sample_interval", "60")
	rr2 := postForm(h, "/config", form2, cookie, csrf)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("bad combo status = %d, want 400, body: %s", rr2.Code, rr2.Body.String())
	}
	if rr2.Body.Len() == 0 {
		t.Error("bad combo response body should surface the validator's error, not swallow it")
	}
	if (*cfg).FastInterval != before.FastInterval || (*cfg).SampleInterval != before.SampleInterval {
		t.Errorf("intervals changed despite bad combo: got %d/%d, want unchanged %d/%d",
			(*cfg).FastInterval, (*cfg).SampleInterval, before.FastInterval, before.SampleInterval)
	}
}

// TestConfigSaveAcceptsValidFinalIntervalCombo pins the fix for the
// interval-validation-order bug: configSaveHandler must validate the FINAL
// (fast_interval, sample_interval) pair as a whole, not each field against
// the other's stale value. From defaults (fast=5, sample=60), POSTing
// fast=7/sample=126 is a valid final combo (126%7==0) even though it fails
// against either field's OLD value in isolation (60%7!=0, 126%5!=0) --
// previously this was wrongly rejected with 400 no matter which field's
// config.Set ran first.
func TestConfigSaveAcceptsValidFinalIntervalCombo(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	if (*cfg).FastInterval != 5 || (*cfg).SampleInterval != 60 {
		t.Fatalf("preconditions: fast/sample = %d/%d, want defaults 5/60", (*cfg).FastInterval, (*cfg).SampleInterval)
	}

	form := baseConfigForm()
	form.Set("fast_interval", "7")
	form.Set("sample_interval", "126")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !*reloadCalled {
		t.Fatal("Reload was not called")
	}
	if (*cfg).FastInterval != 7 || (*cfg).SampleInterval != 126 {
		t.Errorf("intervals = %d/%d, want 7/126", (*cfg).FastInterval, (*cfg).SampleInterval)
	}
	if !strings.Contains(rr.Body.String(), `name="fast_interval" value="7"`) {
		t.Error("re-rendered page doesn't show fast_interval=7")
	}
	if !strings.Contains(rr.Body.String(), `name="sample_interval" value="126"`) {
		t.Error("re-rendered page doesn't show sample_interval=126")
	}
}

// TestConfigSaveRejectsInvalidFinalIntervalCombo is the flip side: a final
// pair that's genuinely invalid (100%7!=0) still 400s with nothing written,
// even though the fix now validates the pair together instead of
// field-by-field.
func TestConfigSaveRejectsInvalidFinalIntervalCombo(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	before := *(*cfg)
	form := baseConfigForm()
	form.Set("fast_interval", "7")
	form.Set("sample_interval", "100")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite an invalid final interval combo")
	}
	if (*cfg).FastInterval != before.FastInterval || (*cfg).SampleInterval != before.SampleInterval {
		t.Errorf("intervals changed despite bad combo: got %d/%d, want unchanged %d/%d",
			(*cfg).FastInterval, (*cfg).SampleInterval, before.FastInterval, before.SampleInterval)
	}
}

// TestConfigSaveFloatFieldsRoundTrip pins two float fields from different
// panels: thresholds.cpu_pct (Thresholds) and baseline_min_pct (Anomaly
// detection).
func TestConfigSaveFloatFieldsRoundTrip(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := baseConfigForm()
	form.Set("cpu_pct", "97.5")
	form.Set("baseline_min_pct", "0.25")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if (*cfg).Thresholds.CPUPct != 97.5 {
		t.Errorf("CPUPct = %v, want 97.5", (*cfg).Thresholds.CPUPct)
	}
	if (*cfg).BaselineMinPct != 0.25 {
		t.Errorf("BaselineMinPct = %v, want 0.25", (*cfg).BaselineMinPct)
	}
}

// TestConfigSaveRetentionDurationRejectsBadValue pins the duration fields
// (storage.raw_retention/rollup_retention): an unparseable duration rejects
// with 400 via config.Config.Set's existing validateRetentionDuration, and
// writes nothing.
func TestConfigSaveRetentionDurationRejectsBadValue(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	before := *(*cfg)
	form := baseConfigForm()
	form.Set("raw_retention", "not-a-duration")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rr.Code, rr.Body.String())
	}
	if *reloadCalled {
		t.Error("Reload was called despite an invalid raw_retention")
	}
	if (*cfg).Storage.RawRetention != before.Storage.RawRetention {
		t.Errorf("RawRetention = %q, want unchanged %q", (*cfg).Storage.RawRetention, before.Storage.RawRetention)
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
		`name="cpu_pct" value="95"`,
		`name="swap_pct" value="50"`,
		`name="baseline_min_pct" value="0.15"`,
		`name="fast_interval" value="5"`,
		`name="sample_interval" value="60"`,
		`name="heartbeat_interval" value="30"`,
		`name="smart_interval" value="1800"`,
		`name="raw_retention" value="48h"`,
		`name="rollup_retention" value="720h"`,
		`disk:/`,
		`action="/config"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config page missing %q:\n%s", want, body)
		}
	}
}

// TestConfigPageWebPanelReadOnlyAndForgedFieldIgnored pins the "Access &
// domain" panel's read-only contract: GET renders the current web.* values,
// but no <input name="..."> in the page can ever Set a web.* key (a forged
// POST field targeting one must be silently ignored) -- the whole point being
// that editing origin/rp_id from the web UI risks locking an admin out of
// passkey login, so it's CLI-only.
func TestConfigPageWebPanelReadOnlyAndForgedFieldIgnored(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Web.Enabled = true
	(*cfg).Web.Mode = "autocert"
	(*cfg).Web.Origin = "https://monitor.home.lan"
	(*cfg).Web.RPID = "monitor.home.lan"
	(*cfg).Web.Listen = "0.0.0.0:8088"
	(*cfg).Web.SessionTTL = "24h"
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	getReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body: %s", getRR.Code, getRR.Body.String())
	}
	body := getRR.Body.String()
	for _, want := range []string{
		"monitor.home.lan",
		"autocert",
		"https://monitor.home.lan",
		"0.0.0.0:8088",
		"24h",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config page missing read-only web.* value %q:\n%s", want, body)
		}
	}
	// No form control in the page may carry a name that config.Config.Set
	// would recognize as a web.* key.
	for _, forbidden := range []string{
		`name="web.rp_id"`, `name="rp_id"`, `name="web_rp_id"`,
		`name="web.origin"`, `name="origin"`, `name="web_origin"`,
		`name="web.mode"`, `name="web_mode"`,
		`name="web.enabled"`, `name="web_enabled"`,
		`name="web.listen"`, `name="web_listen"`,
		`name="web.session_ttl"`, `name="web_session_ttl"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("config page must not expose an editable %s (web.* is read-only)", forbidden)
		}
	}

	// Even if a form submission forges a web.* field (e.g. via a modified
	// request, not something the real page ever sends), the handler must
	// ignore it: it isn't in the scalarEdit list at all.
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	form := baseConfigForm()
	form.Set("web.rp_id", "evil.example.com")
	form.Set("rp_id", "evil.example.com")
	postRR := postForm(h, "/config", form, cookie, csrf)
	if postRR.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200, body: %s", postRR.Code, postRR.Body.String())
	}
	if (*cfg).Web.RPID != "monitor.home.lan" {
		t.Errorf("Web.RPID = %q, want unchanged %q (forged web field must be ignored)", (*cfg).Web.RPID, "monitor.home.lan")
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

// TestConfigPageManagedFieldsDisabledAndPostRejected pins task 8's read-only
// contract on a fleet child with a managed key: GET disables that field and
// shows the fragment id; a POST that never touches it (a real browser never
// submits a disabled field) still saves everything ELSE normally; a POST
// that forges the managed field anyway (bypassing the disabled attribute)
// is rejected outright, with nothing written -- even to an unrelated field
// in the same request.
func TestConfigPageManagedFieldsDisabledAndPostRejected(t *testing.T) {
	d, cfg, reloadCalled := configTestDeps(t)
	(*cfg).Thresholds.CPUPct = 85
	d.Fleet = func() core.FleetAPI {
		return &fakeFleet{status: core.FleetStatus{
			Role: config.RoleChild,
			Link: &core.LinkView{State: "linked", Managed: map[string]string{"thresholds.cpu_pct": "abc123def456"}},
		}}
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	getReq := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config")
	getRR := httptest.NewRecorder()
	h.ServeHTTP(getRR, getReq)
	if getRR.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200, body: %s", getRR.Code, getRR.Body.String())
	}
	body := getRR.Body.String()
	if !strings.Contains(body, `name="cpu_pct" value="85" disabled`) {
		t.Errorf("config page must render cpu_pct disabled while managed:\n%s", body)
	}
	if !strings.Contains(body, "abc123def456") || !strings.Contains(body, "Managed by the fleet master") {
		t.Errorf("config page must show the managing fragment id and note:\n%s", body)
	}

	// A normal save (the managed field simply absent, as a real browser
	// would send it) must succeed and leave the managed field untouched
	// while everything else applies.
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)
	form := baseConfigForm()
	form.Del("cpu_pct")
	rr := postForm(h, "/config", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("normal save status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if (*cfg).Thresholds.CPUPct != 85 {
		t.Errorf("CPUPct = %v, want unchanged 85 (managed field never applied)", (*cfg).Thresholds.CPUPct)
	}
	if (*cfg).Thresholds.MemPct != 85 {
		t.Errorf("MemPct = %v, want 85 (an ordinary field in the same request must still save)", (*cfg).Thresholds.MemPct)
	}

	// A forged POST that includes the managed field anyway is rejected
	// entirely -- nothing is written, not even the unrelated field.
	*reloadCalled = false
	forged := baseConfigForm()
	forged.Set("cpu_pct", "999")
	forged.Set("mem_pct", "42")
	forgedRR := postForm(h, "/config", forged, cookie, csrf)
	if forgedRR.Code != http.StatusBadRequest {
		t.Fatalf("forged POST status = %d, want 400, body: %s", forgedRR.Code, forgedRR.Body.String())
	}
	if !strings.Contains(forgedRR.Body.String(), "managed by the fleet master") {
		t.Errorf("forged POST body = %s, want the managed-by-master message", forgedRR.Body.String())
	}
	if *reloadCalled {
		t.Error("a rejected forged POST must never apply/persist anything")
	}
	if (*cfg).Thresholds.MemPct != 85 {
		t.Errorf("MemPct = %v, want unchanged 85 (the whole forged POST is rejected, not just the managed field)", (*cfg).Thresholds.MemPct)
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
