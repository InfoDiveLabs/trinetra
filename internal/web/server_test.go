package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// testDeps builds a minimal Deps for handler/Start tests: enabled, bound to
// an ephemeral port, with just enough of the Cfg/Reload/Snapshot plumbing
// wired for a zero value to be usable (newHandler's dashboard placeholder
// doesn't read any of these yet, but a future page-handler test reusing this
// helper will want them present).
func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Cfg:      func() *config.Config { return config.Default() },
		Reload:   func(*config.Config) error { return nil },
		Snapshot: func() DashboardView { return DashboardView{} },
		Enabled:  true,
		Listen:   "127.0.0.1:0",
	}
}

// TestBrandShowsServerName pins #101: the sidebar brand subtitle renders the
// configured server.name (from Cfg().ServerName()) instead of the old
// hardcoded MONITOR.HOME.LAN.
func TestBrandShowsServerName(t *testing.T) {
	d := enrollTestDeps(t)
	cfg := config.Default()
	cfg.Name = "attic-pi"
	d.Cfg = func() *config.Config { return cfg }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / (signed in) = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "attic-pi") {
		t.Errorf("brand does not render server.name (missing 'attic-pi')")
	}
	if strings.Contains(body, "MONITOR.HOME.LAN") {
		t.Errorf("old hardcoded brand MONITOR.HOME.LAN still present")
	}
}

// TestHostPageRendersInventory pins #100's web surface: GET /host renders the
// host hardware/OS inventory fetched over the API.
func TestHostPageRendersInventory(t *testing.T) {
	d := enrollTestDeps(t)
	d.API = fakeAPI{hostInfo: core.HostInfoView{
		Hostname: "attic-pi", OS: "Debian GNU/Linux 12", Kernel: "6.1.0-arm64",
		CPUModel: "Cortex-A72", CPUCores: 4, CPUThreads: 4, MemTotalBytes: 8 << 30, UptimeSec: 90061, LocalIP: "192.168.1.50", PublicIP: "203.0.113.7",
		Disks: []core.HostDiskView{{Device: "nvme0n1", Model: "WD SN570", SizeBytes: 512 << 30, FSType: "ext4", Mount: "/"}},
	}}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/host"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /host = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"attic-pi", "Debian GNU/Linux 12", "Cortex-A72", "4 cores", "8.0 GiB", "1d 1h 1m", "nvme0n1", "WD SN570", "SSD", "192.168.1.50", "203.0.113.7"} {
		if !strings.Contains(body, want) {
			t.Errorf("host page missing %q", want)
		}
	}
}

// TestServerServesDashboardAndAssets pins newHandler's routes: GET / is
// viewer+ (requireRole(RoleViewer, ...)), so a SIGNED-IN request renders the
// base layout (brand + nav) around the dashboard placeholder while an
// anonymous one redirects to /login; and GET /assets/style.css serves the
// embedded mockup CSS verbatim (anonymously -- assets aren't gated) with a
// text/css content type. httptest.NewRecorder exercises the handler
// directly, no real port bound.
func TestServerServesDashboardAndAssets(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	// Anonymous GET / must redirect to /login now that the dashboard is
	// viewer+ (requireRole), not render the page.
	anon := httptest.NewRecorder()
	h.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "/", nil))
	if anon.Code != http.StatusFound {
		t.Fatalf("anonymous GET / status = %d, want %d (redirect to /login)", anon.Code, http.StatusFound)
	}
	if loc := anon.Header().Get("Location"); loc != "/login" {
		t.Errorf("anonymous GET / Location = %q, want /login", loc)
	}

	// A signed-in viewer reaches the dashboard placeholder.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / (signed in) status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "serverwatch") {
		t.Errorf("dashboard body missing brand %q:\n%s", "serverwatch", body)
	}
	if !strings.Contains(body, "Dashboard") {
		t.Errorf("dashboard body missing nav item %q:\n%s", "Dashboard", body)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/assets/style.css", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /assets/style.css status = %d, want 200", rr2.Code)
	}
	if ct := rr2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css prefix", ct)
	}
	if rr2.Body.Len() == 0 {
		t.Error("style.css body is empty")
	}
}

// TestDashboardLinksToMonitoringPage pins the reverse of the earlier
// /monitoring 404 fix (284abbd): now that /monitoring is a real, working
// page, the dashboard's "Top containers"/"Filesystems" panels must link to
// it again -- a dead link was worse than no link, but a live link that's
// missing is just as much a regression once the target exists.
func TestDashboardLinksToMonitoringPage(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / (admin) status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `href="/monitoring"`) {
		t.Errorf("dashboard body missing a /monitoring link:\n%s", body)
	}

	// And the link actually resolves: GET /monitoring itself renders for a
	// signed-in viewer rather than 404ing.
	mrr := httptest.NewRecorder()
	h.ServeHTTP(mrr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/monitoring"))
	if mrr.Code != http.StatusOK {
		t.Fatalf("GET /monitoring (viewer) status = %d, want 200, body: %s", mrr.Code, mrr.Body.String())
	}
}

// TestServerServesJSAssetWithApplicationJavascriptType pins the JS content
// type explicitly: http.FileServer's default mime lookup can vary by OS
// mime.types, so newHandler must not rely on it for .js.
func TestServerServesJSAssetWithApplicationJavascriptType(t *testing.T) {
	h := newHandler(testDeps(t))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.js status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/javascript") {
		t.Errorf("content-type = %q, want application/javascript prefix", ct)
	}
	if rr.Body.Len() == 0 {
		t.Error("app.js body is empty")
	}
}

// TestAssetsDirectoryListingSuppressed pins that GET /assets/ (no filename)
// does not leak an http.FileServer directory index of every embedded asset:
// it must 404, while a concrete asset under it still serves 200 with its
// content type.
func TestAssetsDirectoryListingSuppressed(t *testing.T) {
	h := newHandler(testDeps(t))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets/", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/ status = %d, want 404 (no directory listing)", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "style.css") {
		t.Errorf("GET /assets/ leaked a directory listing:\n%s", rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/assets/style.css", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("GET /assets/style.css status = %d, want 200", rr2.Code)
	}
	if ct := rr2.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css prefix", ct)
	}
}

// TestStartBindsWhenEnabled exercises the other half of Start not covered by
// TestStartReturnsNoopStop (Task 1's Enabled=false stub path): when Enabled
// is true, Start must actually bind a listener on Listen and return a stop
// func that shuts it down cleanly.
func TestStartBindsWhenEnabled(t *testing.T) {
	stop, err := Start(testDeps(t))
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if stop == nil {
		t.Fatal("Start returned a nil stop func")
	}
	stop() // must not panic
}

// TestStartReturnsNoopStop pins Start's Enabled=false path (the disabled
// half of the contract the serverwatch-web binary's run func,
// cmd/serverwatch-web/main.go, relies on): no
// listener is bound, and stop/err are still safe to use. See
// TestStartBindsWhenEnabled above for the Enabled=true half.
func TestStartReturnsNoopStop(t *testing.T) {
	stop, err := Start(Deps{
		Enabled: false,
		Listen:  "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if stop == nil {
		t.Fatal("Start returned a nil stop func")
	}
	stop() // must not panic
}
