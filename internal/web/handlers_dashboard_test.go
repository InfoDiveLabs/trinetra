package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/core"
)

// dashboardTestView is a distinctive fake DashboardView: every number is
// chosen so it can't collide with any of the mockup's hard-coded demo
// figures (18% cpu, 83% mem, 4% swap, 0.42 load, 54°C temp, 6/7 containers,
// 220 units, 91% disk, 214 processes, 11%/512M nextcloud, ...) -- see
// ui-mockup/dashboard.html. If a rendered page still shows one of those, the
// template is still using demo markup instead of the real Deps.Snapshot()
// value.
func dashboardTestView() DashboardView {
	return DashboardView{
		TS:      1_700_000_000,
		Online:  true,
		CPU:     37.25,
		MemPct:  61.4,
		SwapPct: 12.5,
		Load1:   1.11,
		Load5:   1.22,
		Load15:  1.33,
		TempC:   47,
		Cores:   6,
		Processes: ProcessCounts{
			Total: 333, Running: 5, Sleeping: 320, Zombie: 2,
		},
		ContainersRunning: 3,
		ContainersTotal:   5,
		TopCPUContainers: []ContainerView{
			{Name: "sw-nextcloud", State: "running", CPUPct: 27.5, MemMiB: 555},
			{Name: "sw-postgres", State: "running", CPUPct: 8.25, MemMiB: 321},
		},
		TopMemContainers: []ContainerView{
			{Name: "sw-postgres", State: "running", CPUPct: 8.25, MemMiB: 321},
			{Name: "sw-nextcloud", State: "running", CPUPct: 27.5, MemMiB: 555},
		},
		UnitsFailed: 3,
		UnitsTotal:  177,
		Disks: []DiskView{
			{Mount: "/srv/swdata", Device: "/dev/swdisk1", UsagePct: 63.5, FreeBytes: 42 * 1 << 30, SizeBytes: 200 * 1 << 30, DaysToFull: 17, DaysToFullKnown: true},
		},
		DisksCritical: 1,
		NetIfaces: []NetIfaceView{
			{Name: "sw-eth9", RxBps: 2_345_678, TxBps: 456_789},
		},
		NetRxBps: 2_345_678,
		NetTxBps: 456_789,
	}
}

// dashboardMockupDemoNumbers are the mockup's hard-coded demo figures
// (ui-mockup/dashboard.html) that must NOT survive into the real dashboard
// template once it's bound to Deps.Snapshot() -- each is distinctive enough
// (in context) not to collide with real formatted output from
// dashboardTestView above.
var dashboardMockupDemoNumbers = []string{
	"6<span class=\"of\">/7", // containers running demo count
	"220<span class=\"of\">", // systemd units demo count
	"12d 04h",                // uptime demo
	"nextcloud</span><span class=\"track\"><i style=\"width:62%\"", // top-cpu demo hbar
	"512M",             // top-mem demo value
	"1 incident · 45m", // availability-strip demo incident/downtime text
}

func dashboardTestDeps(t *testing.T) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.API = fakeAPI{snap: dashboardTestView()}
	return d
}

// TestDashboardRendersRealSnapshotValues pins the core TDD obligation for
// this task: GET / (signed in) must render actual numbers pulled from
// Deps.Snapshot() (through the DashboardView adapter), not the mockup's
// hard-coded demo figures.
func TestDashboardRendersRealSnapshotValues(t *testing.T) {
	d := dashboardTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		"37",                   // CPU
		"61",                   // MemPct
		"1.11", "1.22", "1.33", // load1/5/15
		"47", // temp
		"6 cores",
		"3<span class=\"of\">/5", // containers running/total
		"3<span class=\"of\">",   // failed units
		"/ 177",                  // units total (rendered as "3<span class=\"of\">/ 177")
		"333",                    // process total
		"sw-nextcloud",
		"sw-postgres",
		"/srv/swdata",
		"/dev/swdisk1",
		"sw-eth9",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard body missing real value %q:\n%s", want, body)
		}
	}

	for _, demo := range dashboardMockupDemoNumbers {
		if strings.Contains(body, demo) {
			t.Errorf("dashboard body still contains mockup demo markup %q", demo)
		}
	}
}

// TestDashboardShowsHostStrip pins the compact host strip (#100): when host
// info is available, the dashboard renders the OS, a socket-aware CPU digest,
// memory, uptime, and local IP, with a link through to the full /host page.
func TestDashboardShowsHostStrip(t *testing.T) {
	d := dashboardTestDeps(t)
	d.API = fakeAPI{snap: dashboardTestView(), hostInfo: core.HostInfoView{
		Hostname: "infodivelabs", OS: "Ubuntu 24.04.3 LTS",
		CPUModel: "Intel(R) Xeon(R) Platinum 8153", CPUSockets: 2, CPUCores: 32, CPUThreads: 64,
		MemTotalBytes: 67 << 30, UptimeSec: 90061, LocalIP: "192.168.1.101",
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"hoststrip", "Ubuntu 24.04.3 LTS", "2× Intel(R) Xeon(R) Platinum 8153", "(64t)", "192.168.1.101", `href="/host"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard host strip missing %q:\n%s", want, body)
		}
	}
}

// TestSidebarShowsCoreAndPluginVersions pins #107: the sidebar footer shows the
// core daemon's version (over the socket) and the web plugin's own version, and
// flags a mismatch when they differ. The web plugin's own version is "dev" in a
// test binary, so a distinct core version must render as a mismatch.
func TestSidebarShowsCoreAndPluginVersions(t *testing.T) {
	d := dashboardTestDeps(t)
	d.API = fakeAPI{snap: dashboardTestView(), version: "v9.9.9"}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/"))
	body := rr.Body.String()
	for _, want := range []string{"core v9.9.9", "web dev", "version mismatch"} {
		if !strings.Contains(body, want) {
			t.Errorf("sidebar version block missing %q", want)
		}
	}
}

// TestDashboardRendersRealAvailabilityStrip pins Part 2 of the field-feedback
// fix: the #hbstrip availability panel must render the real per-request
// Availability data (ComputeAvailability's output, threaded through
// DashboardView) rather than the app.js mockup's hardcoded #hbstrip demo
// (N=96/dF=68/dT=70/wA=41, "1 incident · 45m").
func TestDashboardRendersRealAvailabilityStrip(t *testing.T) {
	view := dashboardTestView()
	view.Availability = Availability{
		Blocks:         []AvailabilityBlock{{Down: false, Label: "00:00"}, {Down: true, Label: "00:15"}},
		UptimePct:      97.57,
		Incidents:      2,
		IncidentsLabel: "2 incidents",
		DowntimeStr:    "35m",
	}
	d := enrollTestDeps(t)
	d.API = fakeAPI{snap: view}

	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	for _, want := range []string{
		"97.57% up",
		"2 incidents",
		"35m",
		`class="seg down" title="00:15 · down"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard body missing real availability value %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "1 incident · 45m") {
		t.Error("dashboard body still contains the app.js mockup's hardcoded availability demo text")
	}
}

// TestDashboardRoutesAreViewerGated pins that both GET / and GET /events
// require at least a viewer session: anonymous is redirected to /login. (The
// positive/admin case is already covered by TestServerServesDashboardAndAssets
// for GET / and by the SSE tests in sse_test.go for GET /events; this test
// is the negative case for both routes together, since Task 8 adds /events
// as a second viewer-gated route alongside the existing dashboard gate.)
func TestDashboardRoutesAreViewerGated(t *testing.T) {
	d := dashboardTestDeps(t)
	h := newHandler(d)
	for _, route := range []string{"/", "/events"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, route, nil))
		if rr.Code != http.StatusFound {
			t.Errorf("GET %s anon status = %d, want %d", route, rr.Code, http.StatusFound)
		}
		if loc := rr.Header().Get("Location"); loc != "/login" {
			t.Errorf("GET %s anon Location = %q, want /login", route, loc)
		}
	}
}
