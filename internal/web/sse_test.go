package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/config"
)

// eventsTestDeps builds Deps for the SSE tests: a distinctive fake
// Deps.Snapshot (so a test can assert its value round-trips onto the wire)
// and a Cfg with a deliberately LARGE FastInterval — see
// TestEventsStreamStopsPromptlyOnClientDisconnect's doc for why that matters.
func eventsTestDeps(t *testing.T, fastIntervalSec int) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.Snapshot = func() DashboardView { return DashboardView{CPU: 42.5, MemPct: 61} }
	cfg := config.Default()
	cfg.FastInterval = fastIntervalSec
	d.Cfg = func() *config.Config { return cfg }
	return d
}

// readSSEFrame reads the next "event: <name>\ndata: <json>\n\n" frame off r
// (the exact shape writeSnapshotEvent/writeAlertEvent/writePublicSnapshotEvent
// all write), skipping any stray blank lines first, and returns the event
// name and the data payload with the "data: " prefix and trailing newline
// stripped. Used by every test that feeds Deps.Subscribe's fake channel
// directly and needs to assert on the resulting frame's event name (not just
// its JSON body).
func readSSEFrame(t *testing.T, r *bufio.Reader) (event, data string) {
	t.Helper()
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			dataLine, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("reading SSE data line: %v", err)
			}
			data = strings.TrimSpace(strings.TrimPrefix(dataLine, "data: "))
			return event, data
		}
	}
}

// viewerCookie mints a live viewer session and returns its sw_session
// cookie, for tests that need a real (non-httptest.NewRequest-scoped) HTTP
// client request against an httptest.Server.
func viewerCookie(t *testing.T, users UserStore, sessions SessionStore) *http.Cookie {
	t.Helper()
	u := &User{ID: mustNewUserID(t), Name: "viewer1", Role: RoleViewer, Created: 1}
	if err := users.Put(u); err != nil {
		t.Fatalf("seed viewer Put: %v", err)
	}
	sess, err := sessions.New(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	return &http.Cookie{Name: sessionCookieName, Value: sess.ID}
}

// TestEventsStreamEmitsSnapshotFrame pins /events' core contract: a viewer
// GET gets a text/event-stream response whose very first frame already
// carries the current DashboardView (as JSON) — no waiting for the fast-tier
// ticker's first tick.
func TestEventsStreamEmitsSnapshotFrame(t *testing.T) {
	d := eventsTestDeps(t, 60) // a long tick period the test must not need to wait for
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream prefix", ct)
	}

	type result struct {
		line string
		err  error
	}
	lines := make(chan result, 1)
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if strings.HasPrefix(line, "data: ") {
				lines <- result{line: line}
				return
			}
			if err != nil {
				lines <- result{err: err}
				return
			}
		}
	}()

	select {
	case res := <-lines:
		if res.err != nil {
			t.Fatalf("reading SSE stream: %v", res.err)
		}
		if !strings.Contains(res.line, `"cpu":42.5`) {
			t.Errorf("first SSE data frame = %q, want it to contain the current cpu value (42.5)", res.line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first SSE data frame")
	}
}

// TestEventsStreamIncludesTopContainers pins that /events' SSE frame carries
// the current top-containers CPU/mem data (DashboardView.TopCPUContainers/
// TopMemContainers), not just the resource tiles' scalars — app.js's
// swBootSSE needs this on the wire to keep the "Top containers · CPU"/
// "· Memory" hbar panels live between full page loads, matching the
// server-rendered ones dashboardHandler builds via containerBars
// (handlers_dashboard.go).
func TestEventsStreamIncludesTopContainers(t *testing.T) {
	d := eventsTestDeps(t, 60)
	d.Snapshot = func() DashboardView {
		return DashboardView{
			CPU:              42.5,
			TopCPUContainers: []ContainerView{{Name: "web", State: "running", CPUPct: 37, MemMiB: 128}},
			TopMemContainers: []ContainerView{{Name: "db", State: "running", CPUPct: 4, MemMiB: 512}},
		}
	}
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	var dataLine string
	for {
		line, err := r.ReadString('\n')
		if strings.HasPrefix(line, "data: ") {
			dataLine = line
			break
		}
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
	}

	if !strings.Contains(dataLine, `"top_cpu_containers":[{"name":"web"`) {
		t.Errorf("SSE frame missing top_cpu_containers: %s", dataLine)
	}
	if !strings.Contains(dataLine, `"top_mem_containers":[{"name":"db"`) {
		t.Errorf("SSE frame missing top_mem_containers: %s", dataLine)
	}
}

// TestEventsStreamStopsPromptlyOnClientDisconnect pins that eventsHandler
// notices r.Context().Done() (a client disconnect) immediately rather than
// only discovering it the next time its ticker fires and a write fails.
// FastInterval is set to 60s specifically so that if the implementation
// only relied on the next tick's write erroring out, this test — which
// requires the handler to have returned within a couple of seconds of the
// client going away — would time out.
func TestEventsStreamStopsPromptlyOnClientDisconnect(t *testing.T) {
	d := eventsTestDeps(t, 60)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}

	// Make sure the handler is actually up and streaming before we pull the
	// rug: read past the first frame.
	buf := make([]byte, 512)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("reading first frame: %v", err)
	}

	// Simulate the client going away.
	cancel()
	resp.Body.Close()

	// httptest.Server.Close() blocks until every outstanding request's
	// handler has returned. If eventsHandler didn't select on
	// r.Context().Done() and instead only noticed the disconnect via a
	// failed write on the next tick (60s away), this would hang well past
	// any reasonable deadline — proving the ctx-cancellation path is what
	// actually lets the handler return.
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("srv.Close() did not return within 2s of client disconnect — eventsHandler did not stop promptly on r.Context().Done()")
	}
}

// TestEventsStreamSubscribePushesSnapshotFrameOnSnapshotEvent pins Task 3's
// core push-driven contract: when Deps.Subscribe is set, a Kind:"snapshot"
// LiveEvent fed on the returned channel makes eventsHandler write a fresh
// snapshot SSE frame immediately, rather than waiting for the (now-fallback)
// ticker.
func TestEventsStreamSubscribePushesSnapshotFrameOnSnapshotEvent(t *testing.T) {
	d := eventsTestDeps(t, 60)
	sub := make(chan LiveEvent)
	d.Subscribe = func(ctx context.Context) (<-chan LiveEvent, error) { return sub, nil }
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	readSSEFrame(t, r) // discard the initial connect-time snapshot frame

	sub <- LiveEvent{Kind: "snapshot", Time: 100}

	event, data := readSSEFrame(t, r)
	if event != "snapshot" {
		t.Fatalf("event = %q, want snapshot", event)
	}
	if !strings.Contains(data, `"cpu":42.5`) {
		t.Errorf("snapshot frame = %q, want it to contain the current cpu value", data)
	}
}

// TestEventsStreamSubscribePushesAlertFrameOnAlertEvent pins that a
// non-"snapshot" LiveEvent (an alert fire/recover) makes eventsHandler write
// a distinct "alert" SSE frame carrying the event's fields, instead of a
// snapshot frame.
func TestEventsStreamSubscribePushesAlertFrameOnAlertEvent(t *testing.T) {
	d := eventsTestDeps(t, 60)
	sub := make(chan LiveEvent)
	d.Subscribe = func(ctx context.Context) (<-chan LiveEvent, error) { return sub, nil }
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	readSSEFrame(t, r) // discard the initial connect-time snapshot frame

	sub <- LiveEvent{Kind: "alert_fire", Severity: "critical", Source: "disk", Title: "disk full", Time: 123}

	event, data := readSSEFrame(t, r)
	if event != "alert" {
		t.Fatalf("event = %q, want alert", event)
	}
	for _, want := range []string{`"kind":"alert_fire"`, `"severity":"critical"`, `"source":"disk"`, `"title":"disk full"`, `"time":123`} {
		if !strings.Contains(data, want) {
			t.Errorf("alert frame = %q, missing %q", data, want)
		}
	}
}

// TestEventsStreamFallsBackToTickerWhenSubscribeChannelCloses pins the safety
// net: once the live channel closes (the daemon connection dropped, say),
// eventsHandler must keep the SSE connection alive and fall back to polling
// Deps.Snapshot() on sseFallbackInterval, rather than stalling or tearing the
// stream down. sseFallbackInterval is shrunk for the duration of this test so
// it doesn't need to wait 30 real seconds for the fallback tick.
func TestEventsStreamFallsBackToTickerWhenSubscribeChannelCloses(t *testing.T) {
	orig := sseFallbackInterval
	sseFallbackInterval = 20 * time.Millisecond
	defer func() { sseFallbackInterval = orig }()

	d := eventsTestDeps(t, 60)
	sub := make(chan LiveEvent)
	d.Subscribe = func(ctx context.Context) (<-chan LiveEvent, error) { return sub, nil }
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	readSSEFrame(t, r) // discard the initial connect-time snapshot frame

	close(sub)

	event, data := readSSEFrame(t, r)
	if event != "snapshot" {
		t.Fatalf("event = %q, want snapshot (fallback ticker)", event)
	}
	if !strings.Contains(data, `"cpu":42.5`) {
		t.Errorf("fallback snapshot frame = %q, want it to contain the current cpu value", data)
	}
}
