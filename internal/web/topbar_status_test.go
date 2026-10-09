package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestTopbarStatusCriticalAlertsFiring pins Part 3 of the field-feedback fix:
// N active CRITICAL alerts must drive the topbar to "crit" with real-count
// text, not the old hardcoded "2 alerts firing" regardless of how many were
// actually active. The count is the TOTAL active-alert count (B4, 2026-09-25
// UI audit), not just the critical ones: /alerts, the dashboard's active
// alerts panel and the sidebar Alerts badge all count every active alert
// regardless of severity (NavCounts.Alerts, navCountsFor's doc), so the pill
// must agree with them rather than silently dropping the warnings.
func TestTopbarStatusCriticalAlertsFiring(t *testing.T) {
	alerts := []activeAlertView{
		{Key: "disk:/", Critical: true},
		{Key: "temp", Critical: true},
		{Key: "cpu", Critical: false},
	}
	status, text := topbarStatus(alerts)
	if status != "crit" {
		t.Fatalf("status = %q, want crit", status)
	}
	if text != "3 alerts firing" {
		t.Fatalf("text = %q, want %q", text, "3 alerts firing")
	}
}

// TestTopbarStatusSingleCriticalAlertSingular confirms the 1-alert case reads
// grammatically ("1 alert firing"), not "1 alerts firing".
func TestTopbarStatusSingleCriticalAlertSingular(t *testing.T) {
	status, text := topbarStatus([]activeAlertView{{Key: "disk:/", Critical: true}})
	if status != "crit" || text != "1 alert firing" {
		t.Fatalf("(status,text) = (%q,%q), want (crit, %q)", status, text, "1 alert firing")
	}
}

// TestTopbarStatusOnlyWarningsIsWarn confirms active alerts with no
// CRITICAL severity drive "warn", with a real warning count.
func TestTopbarStatusOnlyWarningsIsWarn(t *testing.T) {
	alerts := []activeAlertView{{Key: "cpu", Critical: false}}
	status, text := topbarStatus(alerts)
	if status != "warn" {
		t.Fatalf("status = %q, want warn", status)
	}
	if text != "1 warning" {
		t.Fatalf("text = %q, want %q", text, "1 warning")
	}
}

// TestTopbarStatusNoActiveAlertsIsOK confirms zero active alerts of any
// severity renders the "All systems normal" ok pill.
func TestTopbarStatusNoActiveAlertsIsOK(t *testing.T) {
	status, text := topbarStatus(nil)
	if status != "ok" || text != "All systems normal" {
		t.Fatalf("(status,text) = (%q,%q), want (ok, %q)", status, text, "All systems normal")
	}
}

// TestLoadActiveAlertsDecodesCritical confirms activeAlertsViaAPI carries the
// per-record severity (core.AlertRecord.Severity, set at fire time from the
// breaching Check's own severity) into activeAlertView.Critical, so
// topbarStatus has real severity to work with: "critical" -> Critical true,
// anything else -> false.
func TestLoadActiveAlertsDecodesCritical(t *testing.T) {
	d := Deps{API: fakeAPI{active: []core.AlertRecord{
		{Key: "disk:/", Time: 1, Source: "full", Severity: "critical"},
		{Key: "cpu", Time: 2, Source: "hot", Severity: "warning"},
	}}}

	alerts := activeAlertsViaAPI(selfReq(), d)
	var gotDisk, gotCPU activeAlertView
	for _, a := range alerts {
		switch a.Key {
		case "disk:/":
			gotDisk = a
		case "cpu":
			gotCPU = a
		}
	}
	if !gotDisk.Critical {
		t.Errorf("disk:/ Critical = false, want true")
	}
	if gotCPU.Critical {
		t.Errorf("cpu Critical = true, want false (absent from JSON -> zero value)")
	}
}

// TestConfigPageTopbarReflectsRealActiveCriticalAlert pins that every page's
// topbar is accurate: GET /config must not pass a literal "ok" status into
// newPageData regardless of what's firing. With a critical alert active on
// disk, the rendered topbar pill must show "crit" styling and a real "1 alert
// firing" count -- not "All systems normal".
func TestConfigPageTopbarReflectsRealActiveCriticalAlert(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{{Key: "disk:/", Time: 1, Source: "disk full", Severity: "critical"}}}

	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `led crit`) {
		t.Errorf("config page topbar missing crit led class:\n%s", body)
	}
	if !strings.Contains(body, "1 alert firing") {
		t.Errorf("config page topbar missing real alert-firing text:\n%s", body)
	}
	if strings.Contains(body, "All systems normal") {
		t.Errorf("config page topbar still shows the stale hardcoded ok text despite an active critical alert:\n%s", body)
	}
}

// TestConfigPageTopbarCountMatchesNavBadge pins B4 (2026-09-25 UI audit)
// end to end: with 1 critical + 1 warning alert active, the topbar pill's
// count must equal navCountsFor's Alerts badge (both from the same
// activeAlertsViaAPI read) -- "2 alerts firing", not "1 alert firing" from
// counting only the critical one.
func TestConfigPageTopbarCountMatchesNavBadge(t *testing.T) {
	d, _, _ := configTestDeps(t)
	d.API = fakeAPI{active: []core.AlertRecord{
		{Key: "disk:/", Time: 1, Source: "disk full", Severity: "critical"},
		{Key: "cpu", Time: 2, Source: "hot", Severity: "warning"},
	}}

	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "2 alerts firing") {
		t.Errorf("config page topbar count doesn't match total active alerts:\n%s", body)
	}
	if strings.Contains(body, "1 alert firing") {
		t.Errorf("config page topbar still undercounts to the critical-only count:\n%s", body)
	}
}

// TestConfigPageTopbarIsOKWithNoActiveAlerts is the negative case: no active
// alerts at all renders the real "ok"/"All systems normal" pill (which,
// before this fix, was ALSO what "ok" hardcoded -- so this pins that the
// real computation doesn't regress the common case).
func TestConfigPageTopbarIsOKWithNoActiveAlerts(t *testing.T) {
	d, _, _ := configTestDeps(t)

	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/config"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /config status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "All systems normal") {
		t.Errorf("config page topbar missing ok text with no active alerts:\n%s", body)
	}
	if !strings.Contains(body, `led ok`) {
		t.Errorf("config page topbar missing ok led class:\n%s", body)
	}
}
