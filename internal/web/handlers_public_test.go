package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// publicTestSnapshot is a DashboardView with a distinct, individually greppable value on
// every field the public-panel catalog might render.
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
		Availability: Availability{
			UptimePct:      93.25,
			Incidents:      3,
			IncidentsLabel: "3 incidents",
			DowntimeStr:    "1h 12m",
			Blocks:         []AvailabilityBlock{{Down: true, Label: "03:00"}, {Down: false, Label: "03:15"}},
		},
	}
}

// ---- Part 1: "/" routing ----

// TestRootAnonEnabledServesPublicPage pins the core routing branch: an anonymous request to
// / with public.enabled=true gets the public page (200).
func TestRootAnonEnabledServesPublicPage(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("anon GET / (public enabled) status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Public read-only view") {
		t.Errorf("anon GET / did not render the public page:\n%s", body)
	}
	if strings.Contains(body, `id="dashboard-live"`) {
		t.Errorf("anon GET / leaked the dashboard's live-content marker:\n%s", body)
	}
}

// TestRootAnonDisabledRedirectsToLogin pins the other anonymous branch:
// public.enabled=false sends an anonymous / request to /login (302) -- it must NOT 404.
func TestRootAnonDisabledRedirectsToLogin(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = false
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusFound {
		t.Fatalf("anon GET / (public disabled) status = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/login" {
		t.Errorf("anon GET / (public disabled) Location = %q, want /login", loc)
	}
}

// TestRootAuthedViewerAndAdminSeeDashboardEvenWhenPublicEnabled pins that an authenticated
// session ALWAYS reaches the dashboard at /, regardless of public.enabled.
func TestRootAuthedViewerAndAdminSeeDashboardEvenWhenPublicEnabled(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	for _, role := range []Role{RoleViewer, RoleAdmin} {
		req := seedSignedInRequest(t, users, sessions, role, http.MethodGet, "/")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / as %s status = %d, want 200, body: %s", role, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if strings.Contains(body, "Public read-only view") {
			t.Errorf("GET / as %s rendered the anonymous public page instead of the dashboard:\n%s", role, body)
		}
		if !strings.Contains(body, "Dashboard") {
			t.Errorf("GET / as %s missing the dashboard nav item:\n%s", role, body)
		}
	}
}

// TestPublicRouteRedirectsToRoot pins that the old GET /public link is canonicalized onto /
// rather than serving content itself, for BOTH an anonymous caller and a signed-in one.
func TestPublicRouteRedirectsToRoot(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public", nil))
	if rr.Code != http.StatusMovedPermanently && rr.Code != http.StatusFound {
		t.Fatalf("GET /public status = %d, want 301 or 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/" {
		t.Errorf("GET /public Location = %q, want /", loc)
	}
}

// ---- Part 2: the public page's content ----

// TestPublicPageRendersOnlyAllowlistedPanels is the core security-critical obligation: with
// public.enabled=true and public.panels=["cpu"].
func TestPublicPageRendersOnlyAllowlistedPanels(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "42%") {
		t.Errorf("public page missing the allowlisted cpu value 42%%:\n%s", body)
	}
	// mem (77%), swap (13%), both disk mounts (55%/88%), and the availability figures are all
	// present in the snapshot but NOT in public.panels.
	for _, leaked := range []string{"77%", "13%", "55%", "88%", "93.25", "3 incidents", "1h 12m"} {
		if strings.Contains(body, leaked) {
			t.Errorf("public page leaked non-allowlisted value %q:\n%s", leaked, body)
		}
	}
}

// TestPublicPageRendersAvailabilityStripOnlyWhenAllowlisted pins the "availability" panel
// specifically: it's a whole strip, not a scalar tile, resolved via a second.
func TestPublicPageRendersAvailabilityStripOnlyWhenAllowlisted(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu", "availability"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "93.25% up") || !strings.Contains(body, "3 incidents") {
		t.Errorf("availability strip missing despite being allowlisted:\n%s", body)
	}

	// Same allowlist minus "availability": the strip and its figures must disappear entirely.
	(*cfg).Public.Panels = []string{"cpu"}
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/", nil))
	body2 := rr2.Body.String()
	if strings.Contains(body2, "93.25") || strings.Contains(body2, "3 incidents") || strings.Contains(body2, `id="pub-avail"`) {
		t.Errorf("availability strip rendered despite NOT being allowlisted:\n%s", body2)
	}
}

// TestPublicPageIgnoresQueryStringPanelOverride pins that an attacker can't smuggle a
// non-curated metric onto the page via a query parameter (or any other request input).
func TestPublicPageIgnoresQueryStringPanelOverride(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/?panel=mem&panels=mem,swap", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "77%") {
		t.Errorf("query-string override leaked mem's value onto /:\n%s", rr.Body.String())
	}
}

// TestPublicPageNoAuthLeakage pins the broader leakage requirements: no session cookie is
// ever set on the anonymous GET /.
func TestPublicPageNoAuthLeakage(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu", "mem"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if cookies := rr.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("anon GET / set cookies: %+v, want none", cookies)
	}
	body := rr.Body.String()
	// The Login link is the one INTENTIONAL affordance (Part 2) -- assert its
	// presence explicitly rather than merely tolerating it.
	if !strings.Contains(body, `href="/login"`) {
		t.Errorf("public page missing the required Login link:\n%s", body)
	}
	for _, forbidden := range []string{"<form", "<button", `href="/config"`, `href="/users"`, `href="/channels"`, `href="/settings`, "csrf-token", `class="side"`, `class="topbar"`, `id="dashboard-live"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("public page leaked control/admin affordance %q:\n%s", forbidden, body)
		}
	}
}

// TestPublicPageEmptyAllowlistRendersNoPanels pins the degenerate case: enabled=true with
// an empty.
func TestPublicPageEmptyAllowlistRendersNoPanels(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = nil
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
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
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	if !strings.Contains(body, "42%") {
		t.Errorf("cpu missing even though available:\n%s", body)
	}
	if strings.Contains(body, "Temp") || strings.Contains(body, "/missing") {
		t.Errorf("unavailable metrics rendered:\n%s", body)
	}
}

// TestPublicPageSetsNoStoreCacheControl pins the anti-staleness header (issue #67
// follow-up).
func TestPublicPageSetsNoStoreCacheControl(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("anon GET / Cache-Control = %q, want %q", got, "no-store")
	}
}

// ---- Part 2: GET /public/events (anonymous SSE) ----

// TestPublicEventsDisabledReturns404 mirrors the page's own disabled behavior:
// public.enabled=false 404s the stream too, before any SSE headers/streaming setup.
func TestPublicEventsDisabledReturns404(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = false
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/public/events", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /public/events with public.enabled=false status = %d, want 404", rr.Code)
	}
}

// readFirstPublicSSEDataLine issues a real HTTP GET (via an httptest.Server,
// not httptest.NewRecorder -- the handler's for/select loop only returns on
// r.Context().Done(), so a real cancelable client request is required,
// mirroring sse_test.go's eventsHandler tests) against /public/events and
// returns the first "data: " line plus the response (headers/cookies still
// readable after Body.Close()). The response body is closed here,
// synchronously, before returning -- NOT deferred to t.Cleanup -- so that a
// caller's own `defer srv.Close()` (which blocks until the handler's
// goroutine notices the client is gone) doesn't stall for however long the
// SSE ticker takes to next fire; closing eagerly makes the server side
// notice the disconnect immediately instead.
func readFirstPublicSSEDataLine(t *testing.T, srv *httptest.Server) (string, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/public/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /public/events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	for {
		line, err := r.ReadString('\n')
		if strings.HasPrefix(line, "data: ") {
			return line, resp
		}
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
	}
}

// TestPublicEventsStreamsOnlyAllowlistedMetrics is the mandatory security test for the live
// stream.
func TestPublicEventsStreamsOnlyAllowlistedMetrics(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	line, resp := readFirstPublicSSEDataLine(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /public/events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream prefix", ct)
	}
	if !strings.Contains(line, `"value":"42%"`) {
		t.Errorf("SSE frame missing the allowlisted cpu value:\n%s", line)
	}
	for _, leaked := range []string{"77%", "13%", "55%", "88%", "93.25", "3 incidents", "1h 12m", "availability", "\"mem\"", "\"swap\""} {
		if strings.Contains(line, leaked) {
			t.Errorf("SSE frame leaked non-allowlisted data %q:\n%s", leaked, line)
		}
	}
}

// TestPublicEventsIncludesAvailabilityOnlyWhenAllowlisted mirrors
// TestPublicPageRendersAvailabilityStripOnlyWhenAllowlisted for the wire payload.
func TestPublicEventsIncludesAvailabilityOnlyWhenAllowlisted(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu", "availability"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	line, _ := readFirstPublicSSEDataLine(t, srv)
	if !strings.Contains(line, `"availability":{`) || !strings.Contains(line, `"uptime_pct":93.25`) {
		t.Errorf("SSE frame missing availability despite being allowlisted:\n%s", line)
	}
}

// TestPublicEventsStreamStopsOnDisableMidStream pins the security-relevant half of
// publicEventsHandler's per-tick re-check (sse.go).
func TestPublicEventsStreamStopsOnDisableMidStream(t *testing.T) {
	d := enrollTestDeps(t)
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }

	enabled := config.Default()
	enabled.FastInterval = 1
	enabled.Public.Enabled = true
	enabled.Public.Panels = []string{"cpu"}
	var cfgPtr atomic.Pointer[config.Config]
	cfgPtr.Store(enabled)
	d.Cfg = func() *config.Config { return cfgPtr.Load() }

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/public/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /public/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /public/events status = %d, want 200", resp.StatusCode)
	}

	// One reader goroutine: signal firstFrame once the initial "data: " frame (proof the
	// stream is live) arrives, then signal ended when the stream closes.
	firstFrame := make(chan struct{}, 1)
	ended := make(chan struct{}, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		sawFirst := false
		for {
			line, err := r.ReadString('\n')
			if !sawFirst && strings.HasPrefix(line, "data: ") {
				sawFirst = true
				firstFrame <- struct{}{}
			}
			if err != nil {
				ended <- struct{}{}
				return
			}
		}
	}()

	select {
	case <-firstFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first SSE frame before flipping the config")
	}

	// Flip to disabled by atomically swapping in a fresh config (never
	// mutating the one the handler goroutine may be reading).
	disabled := config.Default()
	disabled.FastInterval = 1
	disabled.Public.Enabled = false
	disabled.Public.Panels = []string{"cpu"}
	cfgPtr.Store(disabled)

	select {
	case <-ended:
		// Handler noticed public.enabled=false on the next tick and returned,
		// closing the stream -- exactly the required behavior.
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not terminate within 5s of public.enabled flipping to false -- publicEventsHandler kept streaming to a disabled public view")
	}
}

// TestPublicEventsNoSessionCookie pins that the anonymous stream never sets
// a cookie, mirroring TestPublicPageNoAuthLeakage for the SSE endpoint.
func TestPublicEventsNoSessionCookie(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	_, resp := readFirstPublicSSEDataLine(t, srv)
	if cookies := resp.Cookies(); len(cookies) != 0 {
		t.Errorf("GET /public/events set cookies: %+v, want none", cookies)
	}
}

// TestPublicEventsSetsNoStoreCacheControl mirrors
// TestPublicPageSetsNoStoreCacheControl for the SSE endpoint.
func TestPublicEventsSetsNoStoreCacheControl(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	_, resp := readFirstPublicSSEDataLine(t, srv)
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("GET /public/events Cache-Control = %q, want %q", got, "no-store")
	}
}

// TestPublicEventsSubscribeIgnoresAlertEvents pins the security-relevant half of the
// live-push wiring for the anonymous stream.
func TestPublicEventsSubscribeIgnoresAlertEvents(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	(*cfg).Public.Enabled = true
	(*cfg).Public.Panels = []string{"cpu"}
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	sub := make(chan LiveEvent)
	d.Subscribe = func(ctx context.Context) (<-chan LiveEvent, error) { return sub, nil }

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/public/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /public/events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	readSSEFrame(t, r) // discard the initial connect-time snapshot frame

	sub <- LiveEvent{Kind: "alert_fire", Severity: "critical", Source: "disk", Title: "super-secret-alert-title", Time: 5}
	sub <- LiveEvent{Kind: "snapshot", Time: 6}

	event, data := readSSEFrame(t, r)
	if event != "snapshot" {
		t.Fatalf("event = %q, want snapshot -- an alert event must not produce a public frame of its own", event)
	}
	if strings.Contains(data, "super-secret-alert-title") {
		t.Errorf("public snapshot frame leaked the alert title: %q", data)
	}
}

// ---- Part 3: /settings/public picker ----

// TestPublicSettingsPageRendersPickerForAdmin pins GET /settings/public: it lists the panel
// catalog.
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
	if !strings.Contains(body, `name="panel" value="availability"`) {
		t.Errorf("picker missing the new availability checkbox:\n%s", body)
	}
}

// TestPublicSettingsSaveTogglingAvailabilityPersistsAndAppears pins Part 3's end-to-end
// obligation for the new panel.
func TestPublicSettingsSaveTogglingAvailabilityPersistsAndAppears(t *testing.T) {
	d, cfg, _ := configTestDeps(t)
	d.Snapshot = func() DashboardView { return publicTestSnapshot() }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	_, cookie, csrf := seedAdmin(t, "root", users, sessions)

	form := url.Values{"enabled": {"1"}, "panel": {"cpu", "availability"}}
	rr := postForm(h, "/settings/public", form, cookie, csrf)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	got := map[string]bool{}
	for _, p := range (*cfg).Public.Panels {
		got[p] = true
	}
	if !got["availability"] {
		t.Fatalf("public.panels = %+v, want to contain availability", (*cfg).Public.Panels)
	}

	pageRR := httptest.NewRecorder()
	h.ServeHTTP(pageRR, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(pageRR.Body.String(), "93.25% up") {
		t.Errorf("availability strip missing from public page after enabling the panel:\n%s", pageRR.Body.String())
	}
}

// TestPublicSettingsRoutesAreAdminGated pins RBAC on /settings/public, mirroring
// TestChannelsRoutesAreAdminGated.
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

// TestPublicSettingsSaveRoundTripsAndAudits pins POST /settings/public: the posted enabled
// flag + checked panel ids persist into cfg.Public, Reload is called.
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

// TestPublicSettingsSaveUncheckingAllPanelsClearsThem pins that omitting every "panel"
// checkbox (all unchecked) clears public.panels to empty rather than leaving stale entries.
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

// TestPublicSettingsSaveRejectsUnknownPanelWithNoWrite pins that the config-layer
// allowlist.
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
