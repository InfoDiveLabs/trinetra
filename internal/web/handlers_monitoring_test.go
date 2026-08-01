package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// monitoringTestView is a distinctive fake MonitoringView: every value is
// chosen so it can't collide with any of the mockup's hard-coded demo
// figures (see ui-mockup/monitoring.html: jellyfin/nextcloud/postgres
// containers, docker.service/ssh.service units, /, /data filesystems,
// postgres/php-fpm processes, 220 units, 214 processes, ...). If a rendered
// page still shows one of those, the template is still using demo markup
// instead of the real Deps.Monitoring() value.
func monitoringTestView() MonitoringView {
	return MonitoringView{
		Containers: []MonitoringContainerView{
			{Name: "sw-app1", State: "running", CPUPct: 12.5, MemMiB: 256, NetRxMB: 1.5, NetTxMB: 0.5, HasStats: true},
			{Name: "sw-app2", State: "exited", HasStats: false},
		},
		FailedUnits:      []string{"sw-broken.service"},
		Units:            []MonitoringUnitView{{Name: "sw-good.service", Load: "loaded", Active: "active", Sub: "running", Description: "sw test unit"}},
		UnitsEnabled:     true,
		Processes:        []MonitoringProcessView{{PID: 4242, Name: "sw-proc", State: "running", CPUPct: 8.5, MemMiB: 128, Threads: 4}},
		ProcessesEnabled: true,
		ProcessesTotal:   99,
		Disks: []MonitoringDiskView{
			{Mount: "/sw-data", Device: "/dev/swx1", FSType: "ext4", UsagePct: 55.5, InodePct: 12.5, FreeBytes: 10 * 1 << 30, SizeBytes: 100 * 1 << 30},
		},
	}
}

func monitoringTestDeps(t *testing.T) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.API = fakeAPI{monitoring: monitoringTestView()}
	return d
}

// TestMonitoringRendersRealValues pins the core TDD obligation for Part 1:
// GET /monitoring (signed in) must render actual values pulled from
// Deps.Monitoring(), not the mockup's hard-coded demo figures.
func TestMonitoringRendersRealValues(t *testing.T) {
	d := monitoringTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /monitoring status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		"sw-app1", "sw-app2",
		"sw-broken.service", "sw-good.service", "sw test unit",
		"sw-proc", "4242",
		"/sw-data", "/dev/swx1", "ext4",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("monitoring body missing real value %q:\n%s", want, body)
		}
	}

	for _, demo := range []string{"jellyfin", "nextcloud:29", "docker.service", "Showing 7 of 220 units"} {
		if strings.Contains(body, demo) {
			t.Errorf("monitoring body still contains mockup demo markup %q", demo)
		}
	}
}

// TestMonitoringDisabledCollectorsShowNote pins the "collector disabled" note
// requirement: when collect.services/collect.processes are off, the page
// must render a note explaining that, not a 500 and not a table that looks
// like "zero units/processes".
func TestMonitoringDisabledCollectorsShowNote(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{monitoring: MonitoringView{
		FailedUnits:      nil,
		UnitsEnabled:     false,
		ProcessesEnabled: false,
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /monitoring status = %d, want 200 (not a 500), body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "collector disabled (collect.services)") {
		t.Errorf("monitoring body missing services-disabled note:\n%s", body)
	}
	if !strings.Contains(body, "collector disabled (collect.processes)") {
		t.Errorf("monitoring body missing processes-disabled note:\n%s", body)
	}
}

// TestMonitoringRouteIsViewerGated pins that GET /monitoring requires at
// least a viewer session: anonymous is redirected to /login, exactly like
// the other "Monitor" routes (dashboard/history/alerts).
func TestMonitoringRouteIsViewerGated(t *testing.T) {
	d := monitoringTestDeps(t)
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/monitoring", nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("GET /monitoring anon status = %d, want %d", rr.Code, http.StatusFound)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("GET /monitoring anon Location = %q, want /login", loc)
	}
}

// TestMonitoringFailedUnitsAlwaysShown pins that failed units are always
// listed (systemctl --failed is always collected, regardless of the opt-in
// collect.services full-inventory toggle) — even while the full unit
// inventory table itself shows the "collector disabled" note.
func TestMonitoringFailedUnitsAlwaysShown(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{monitoring: MonitoringView{
		FailedUnits:  []string{"sw-always-shown.service"},
		UnitsEnabled: false,
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /monitoring status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "sw-always-shown.service") {
		t.Errorf("monitoring body missing always-collected failed unit:\n%s", rr.Body.String())
	}
}
