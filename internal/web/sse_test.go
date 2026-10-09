package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// eventsTestDeps builds Deps for the SSE tests: a distinctive fake Deps.Snapshot.
func eventsTestDeps(t *testing.T, fastIntervalSec int) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.Snapshot = func() DashboardView { return DashboardView{CPU: 42.5, MemPct: 61} }
	cfg := config.Default()
	cfg.FastInterval = fastIntervalSec
	d.Cfg = func() *config.Config { return cfg }
	return d
}

// readSSEFrame reads the next "event: <name>\ndata: <json>\n\n" frame off r (the exact
// shape writeSnapshotEvent/writeAlertEvent/writePublicSnapshotEvent all write).
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

// viewerCookie mints a live viewer session and returns its sw_session cookie, for tests
// that need a real.
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

// TestEventsStreamEmitsSnapshotFrame pins /events' core contract.
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

// TestEventsStreamIncludesTopContainers pins that /events' SSE frame carries the current
// top-containers CPU/mem data (DashboardView.TopCPUContainers/ TopMemContainers).
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

// TestEventsStreamStopsPromptlyOnClientDisconnect pins that eventsHandler notices
// r.Context().Done().
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

	// httptest.Server.Close() blocks until every outstanding request's handler has returned.
	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("srv.Close() did not return within 2s of client disconnect -- eventsHandler did not stop promptly on r.Context().Done()")
	}
}

// TestEventsStreamSubscribePushesSnapshotFrameOnSnapshotEvent pins the push-driven
// contract: when Deps.Subscribe is set.
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

// TestEventsStreamSubscribePushesAlertFrameOnAlertEvent pins that a non-"snapshot"
// LiveEvent.
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

// TestEventsStreamFallsBackToTickerWhenSubscribeChannelCloses pins the safety net: once the
// live channel closes (the daemon connection dropped, say).
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

// ---- /n/{node}/events (remote fleet node, poll-only) -------------------

// nodeSnapshotAPI is a core.API test double whose Snapshot() reads a mutable, mutex-guarded
// DashboardView.
type nodeSnapshotAPI struct {
	fakeAPI
	mu   sync.Mutex
	snap core.DashboardView
	err  error
}

func (n *nodeSnapshotAPI) Snapshot() (core.DashboardView, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return core.DashboardView{}, n.err
	}
	return n.snap, nil
}

func (n *nodeSnapshotAPI) setSnapshot(v core.DashboardView) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.snap = v
}

// setErr makes every subsequent Snapshot() call fail with err (nil clears it).
func (n *nodeSnapshotAPI) setErr(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.err = err
}

// nodeEventsTestDeps builds Deps for the node-scoped SSE tests: a master daemon
// (masterFleetWithChild's roster, node_scope_test.go) whose child1 node resolves to child.
func nodeEventsTestDeps(t *testing.T, child core.API, fastIntervalSec int) Deps {
	t.Helper()
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), map[string]core.API{"child1": child}))
	cfg := config.Default()
	cfg.FastInterval = fastIntervalSec
	d.Cfg = func() *config.Config { return cfg }
	return d
}

// TestEventsStreamRemoteNodePollsChildSnapshot pins that GET /n/child1/events streams an
// initial "snapshot" frame built from the CHILD node's own core.API.Snapshot().
func TestEventsStreamRemoteNodePollsChildSnapshot(t *testing.T) {
	child := &nodeSnapshotAPI{snap: core.DashboardView{CPU: 77}}
	d := nodeEventsTestDeps(t, child, 1)
	subscribeCalled := false
	d.Subscribe = func(ctx context.Context) (<-chan LiveEvent, error) {
		subscribeCalled = true
		return nil, nil
	}
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/n/child1/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /n/child1/events: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /n/child1/events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream prefix", ct)
	}

	r := bufio.NewReader(resp.Body)
	event, data := readSSEFrame(t, r)
	if event != "snapshot" {
		t.Fatalf("first event = %q, want snapshot", event)
	}
	if !strings.Contains(data, `"cpu":77`) {
		t.Errorf("initial frame = %q, want it to contain child1's cpu value (77)", data)
	}

	child.setSnapshot(core.DashboardView{CPU: 88})

	event, data = readSSEFrame(t, r)
	if event != "snapshot" {
		t.Fatalf("second event = %q, want snapshot", event)
	}
	if !strings.Contains(data, `"cpu":88`) {
		t.Errorf("second frame = %q, want it to reflect child1's changed cpu value (88)", data)
	}

	if subscribeCalled {
		t.Error("a remote node's /events stream must never call Deps.Subscribe")
	}
}

// TestEventsStreamRemoteNodeStaleAfterThreeErrorsAndClearsOnRecovery pins the stale
// indicator.
func TestEventsStreamRemoteNodeStaleAfterThreeErrorsAndClearsOnRecovery(t *testing.T) {
	child := &nodeSnapshotAPI{snap: core.DashboardView{CPU: 77}}
	d := nodeEventsTestDeps(t, child, 1)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)
	cookie := viewerCookie(t, users, sessions)

	srv := httptest.NewServer(newHandler(d))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/n/child1/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /n/child1/events: %v", err)
	}
	defer resp.Body.Close()

	r := bufio.NewReader(resp.Body)
	beforeErrors := time.Now()
	event, data := readSSEFrame(t, r)
	if event != "snapshot" || !strings.Contains(data, `"cpu":77`) {
		t.Fatalf("initial event = %q data = %q, want snapshot with cpu=77", event, data)
	}

	child.setErr(errors.New("child unreachable"))
	// The 3rd consecutive failed poll is the FIRST thing written to the wire after the initial
	// frame (the 1st/2nd failures write nothing at all -- the "hold" behavior).
	event, data = readSSEFrame(t, r)
	if event != "stale" {
		t.Fatalf("event after 3 consecutive errors = %q, want stale (data: %s)", event, data)
	}
	var stale struct {
		LastSuccess int64 `json:"last_success"`
	}
	if err := json.Unmarshal([]byte(data), &stale); err != nil {
		t.Fatalf("decode stale event: %v (data: %s)", err, data)
	}
	if stale.LastSuccess < beforeErrors.Unix() || stale.LastSuccess > time.Now().Unix() {
		t.Errorf("stale.LastSuccess = %d, want it between %d and now", stale.LastSuccess, beforeErrors.Unix())
	}

	child.setErr(nil) // recovery, same cpu=77 value as before the errors
	event, data = readSSEFrame(t, r)
	if event != "snapshot" || !strings.Contains(data, `"cpu":77`) {
		t.Fatalf("event after recovery = %q data = %q, want a snapshot frame (unchanged data still emitted to clear the stale banner)", event, data)
	}
}

// TestAppJSHandlesStaleSSEEvent is a static source test (this package's
// established convention for app.js, e.g. templates_node_test.go's
// TestAppJSDataFetchesGoThroughNodeURL) pinning that swBootSSE listens for
// the "stale" SSE event, shows the exact banner text, and clears it on the
// next "snapshot" event.
func TestAppJSHandlesStaleSSEEvent(t *testing.T) {
	b, err := assetsFS.ReadFile("assets/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `addEventListener('stale'`) {
		t.Error("app.js missing an SSE 'stale' event listener")
	}
	if !strings.Contains(src, "live updates paused") {
		t.Error("app.js missing the stale banner's \"live updates paused\" text")
	}
	if !strings.Contains(src, "clearStaleBanner()") {
		t.Error("app.js missing a call to clear the stale banner on the next snapshot")
	}
}

// TestEventsStreamSelfUnaffectedByRemoteNodePolling is a narrow sanity
// check that the plain, unprefixed /events request still takes the
// self-scope path (Deps.Snapshot, not apiFor/NodeAPI) even on a Deps wired
// with node routing, as TestEventsStreamEmitsSnapshotFrame et al. already
// pin.
func TestEventsStreamSelfUnaffectedByRemoteNodePolling(t *testing.T) {
	master := masterFakeAPI(masterFleetWithChild(), map[string]core.API{
		"child1": &nodeSnapshotAPI{snap: core.DashboardView{CPU: 5}},
	})
	d := fleetTestDeps(t, master)
	cfg := config.Default()
	cfg.FastInterval = 60
	d.Cfg = func() *config.Config { return cfg }
	d.Snapshot = func() DashboardView { return DashboardView{CPU: 42.5} }
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
	_, data := readSSEFrame(t, r)
	if !strings.Contains(data, `"cpu":42.5`) {
		t.Errorf("self /events frame = %q, want the master's own Deps.Snapshot value (42.5), not child1's", data)
	}
}
