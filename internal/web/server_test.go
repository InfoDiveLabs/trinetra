//go:build web

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"serverwatch/internal/config"
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

// TestServerServesDashboardAndAssets pins newHandler's routes: GET / is
// viewer+ (requireRole(RoleViewer, ...)), so a SIGNED-IN request renders the
// base layout (brand + nav) around the dashboard placeholder while an
// anonymous one redirects to /login; and GET /assets/style.css serves the
// embedded mockup CSS verbatim (anonymously — assets aren't gated) with a
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

// TestNavHasNoMonitoringLink pins the /monitoring 404 fix: that page was
// never built, so the nav (and dashboard) must not link to it. Checks both
// the rendered nav markup and navForRole/navItems directly, so a future
// regression is caught whether it's reintroduced via the nav list or a
// stray template link.
func TestNavHasNoMonitoringLink(t *testing.T) {
	for _, role := range []string{"viewer", "admin"} {
		for _, item := range navForRole(role) {
			if item.Href == "/monitoring" {
				t.Errorf("navForRole(%q) still has a /monitoring entry: %+v", role, item)
			}
		}
	}

	d := enrollTestDeps(t)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / (admin) status = %d, want 200", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, `href="/monitoring"`) {
		t.Errorf("dashboard body still contains a /monitoring link:\n%s", body)
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
// half of the contract internal/serverwatch/daemon_web.go relies on): no
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
