package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newMasterFixtureWithServerTimeout is newMasterFixture but with the
// underlying httptest.Server's Read/WriteTimeout set to wt, so a test can
// prove the stream handler survives a server-wide timeout far shorter than
// the stream itself by clearing its own deadlines.
func newMasterFixtureWithServerTimeout(t *testing.T, wt time.Duration, opts ...func(*MasterConfig)) *masterFixture {
	t.Helper()
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, _ := OpenRegistry(filepath.Join(dir, "registry.json"))
	toks, _ := OpenTokens(filepath.Join(dir, "tokens.json"))
	f := &masterFixture{ca: ca, reg: reg, toks: toks, sink: newFakeSink(), pin: SPKIPin(ca.Cert)}
	mc := MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: f.sink}
	for _, o := range opts {
		o(&mc)
	}
	m := NewMaster(mc)
	srv := httptest.NewUnstartedServer(m.Handler())
	srv.TLS = ServerTLS(leaf, ca.Cert)
	srv.Config.ReadTimeout = wt
	srv.Config.WriteTimeout = wt
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

// newOldMasterFixture is a masterFixture whose handler answers every request
// except PathStream exactly like a real master, and 404s PathStream: this is
// what a pre-phase-2 master looks like to a phase-2 child.
func newOldMasterFixture(t *testing.T) *masterFixture {
	t.Helper()
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, _ := OpenRegistry(filepath.Join(dir, "registry.json"))
	toks, _ := OpenTokens(filepath.Join(dir, "tokens.json"))
	f := &masterFixture{ca: ca, reg: reg, toks: toks, sink: newFakeSink(), pin: SPKIPin(ca.Cert)}
	m := NewMaster(MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: f.sink})
	old := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathStream {
			http.NotFound(w, r)
			return
		}
		m.Handler().ServeHTTP(w, r)
	})
	f.srv = newTLSServer(t, ca, leaf, old)
	return f
}

// startShipperWithFrames joins f and starts a Shipper with OnFrame wired to
// onFrame, returning the shipper, its node id, and a cancel func.
func startShipperWithFrames(t *testing.T, f *masterFixture, onFrame func(Frame), fast bool) (*Shipper, string, context.CancelFunc) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet-child")
	res, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := OpenOutbox(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob,
		Live:      func() (LiveUpdate, error) { return LiveUpdate{Version: "v1", Snapshot: json.RawMessage(`{}`)}, nil },
		LiveEvery: 50 * time.Millisecond,
		OnFrame:   onFrame,
	})
	if fast {
		sh.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	}
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	t.Cleanup(cancel)
	return sh, res.NodeID, cancel
}

func TestStreamPushDeliversFrameWithinOneSecond(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	frames := make(chan Frame, 1)
	_, id, _ := startShipperWithFrames(t, f, func(fr Frame) { frames <- fr }, false)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	if !hub.Push(id, Frame{Type: "lease", Data: json.RawMessage(`{"until":123}`)}) {
		t.Fatal("push to a connected node returned false")
	}
	select {
	case fr := <-frames:
		if fr.Type != "lease" || string(fr.Data) != `{"until":123}` {
			t.Fatalf("frame = %+v", fr)
		}
	case <-time.After(time.Second):
		t.Fatal("frame not delivered within 1s")
	}
}

func TestStreamOnConnectFiresBeforeHandlerDrains(t *testing.T) {
	hub := NewHub(nil)
	var gotConnect []string
	var mu sync.Mutex
	hub.OnConnect(func(nodeID string) {
		mu.Lock()
		gotConnect = append(gotConnect, nodeID)
		mu.Unlock()
		// A Push from inside OnConnect must reach this connection: this is
		// how the master sends lease/silences/managed_config on connect.
		hub.Push(nodeID, Frame{Type: "silences", Data: json.RawMessage(`[]`)})
	})
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	frames := make(chan Frame, 4)
	_, id, _ := startShipperWithFrames(t, f, func(fr Frame) { frames <- fr }, false)

	select {
	case fr := <-frames:
		if fr.Type != "silences" {
			t.Fatalf("frame = %+v", fr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("on-connect push not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gotConnect) == 0 || gotConnect[0] != id {
		t.Fatalf("OnConnect calls = %v, want [%s, ...]", gotConnect, id)
	}
}

func TestStreamPingKeepsConnectionAliveAndIsNotExposed(t *testing.T) {
	old := pingInterval
	pingInterval = 20 * time.Millisecond
	t.Cleanup(func() { pingInterval = old })

	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	var mu sync.Mutex
	var frames []Frame
	_, id, _ := startShipperWithFrames(t, f, func(fr Frame) {
		mu.Lock()
		frames = append(frames, fr)
		mu.Unlock()
	}, false)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	time.Sleep(200 * time.Millisecond) // several ping intervals
	if !hub.Connected(id) {
		t.Fatal("connection dropped while only pings were flowing")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, fr := range frames {
		if fr.Type == "ping" {
			t.Fatal("a ping frame leaked to OnFrame")
		}
	}
}

func TestStreamSurvivesServerWriteTimeout(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixtureWithServerTimeout(t, time.Second, func(c *MasterConfig) { c.Hub = hub })
	frames := make(chan Frame, 16)
	_, id, _ := startShipperWithFrames(t, f, func(fr Frame) { frames <- fr }, false)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	deadline := time.Now().Add(3 * time.Second)
	sent := 0
	for time.Now().Before(deadline) {
		if hub.Push(id, Frame{Type: "lease"}) {
			sent++
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sent == 0 {
		t.Fatal("no frames were pushed")
	}
	if !hub.Connected(id) {
		t.Fatal("stream was killed by the fixture's 1s server WriteTimeout")
	}
}

func TestStreamChildReconnectsAfterMasterDrop(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	_, id, _ := startShipperWithFrames(t, f, func(Frame) {}, true)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	hub.Disconnect(id)
	waitFor(t, "disconnected", func() bool { return !hub.Connected(id) })
	waitFor(t, "reconnected", func() bool { return hub.Connected(id) })
}

func TestStreamRevokedFrameEndsLinkRevoked(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	sh, id, _ := startShipperWithFrames(t, f, func(Frame) {}, true)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	if !hub.Push(id, Frame{Type: "revoked"}) {
		t.Fatal("push of revoked frame failed")
	}
	waitFor(t, "link revoked", func() bool { return sh.Status().State == LinkRevoked })
}

func TestStream403EndsLinkRevoked(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	sh, id, _ := startShipperWithFrames(t, f, func(Frame) {}, true)

	waitFor(t, "connected", func() bool { return hub.Connected(id) })
	if err := f.reg.Update(id, func(n *Node) error { n.Revoked = true; return nil }); err != nil {
		t.Fatal(err)
	}
	hub.Disconnect(id) // force a reconnect attempt, which now hits 403
	waitFor(t, "link revoked", func() bool { return sh.Status().State == LinkRevoked })
}

func TestStreamPushToDisconnectedNodeReturnsFalse(t *testing.T) {
	hub := NewHub(nil)
	if hub.Push("no-such-node", Frame{Type: "ping"}) {
		t.Fatal("push to an unconnected node returned true")
	}
}

func TestHubQueueOverflowDropsWithoutBlockingThePusher(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	hub := NewHub(func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(f, a...))
		mu.Unlock()
	})
	conn := hub.connect("n1") // a "connected" node that never drains its queue
	defer hub.release("n1", conn)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < hubQueueSize+50; i++ {
			hub.Push("n1", Frame{Type: "lease"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Push blocked once the per-node queue filled up")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("expected at least one rate-limited drop log line")
	}
}

func TestStreamOldMaster404LogsOnceAndLeavesDataAndLiveLanesAlone(t *testing.T) {
	f := newOldMasterFixture(t)
	var mu sync.Mutex
	var logs []string
	dir := filepath.Join(t.TempDir(), "fleet-child")
	if _, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := OpenOutbox(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, ob, 1, 5)
	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob,
		Live:      func() (LiveUpdate, error) { return LiveUpdate{Version: "v1", Snapshot: json.RawMessage(`{}`)}, nil },
		LiveEvery: 30 * time.Millisecond,
		OnFrame:   func(Frame) {},
		Logf: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	})
	sh.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sh.Run(ctx)

	waitFor(t, "outbox drained despite no stream route", func() bool { return ob.Acked() == 5 })
	waitFor(t, "linked", func() bool { return sh.Status().State == LinkLinked })
	time.Sleep(250 * time.Millisecond) // several stream-retry attempts at "max backoff"

	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range logs {
		if strings.Contains(l, "old master") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("old-master log lines = %d (%v), want exactly 1", n, logs)
	}
	if sh.Status().State != LinkLinked {
		t.Fatalf("state = %q, want linked (data/live lanes unaffected by the missing stream route)", sh.Status().State)
	}
}

func TestRPCDeliversBodyWithCallerNodeIDNotFromBody(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	id, c := f.join(t, nil)

	done := make(chan struct{})
	var gotNode, gotID string
	var gotBody []byte
	hub.OnRPCResult(func(nodeID, rpcID string, body []byte) {
		gotNode, gotID, gotBody = nodeID, rpcID, body
		close(done)
	})

	resp, err := c.Post(f.srv.URL+PathRPC+"abc123", "application/octet-stream", bytes.NewReader([]byte(`{"claims_node":"someone-else","ok":true}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnRPCResult not called")
	}
	if gotNode != id {
		t.Fatalf("node id = %q, want the mTLS node id %q (never taken from the body)", gotNode, id)
	}
	if gotID != "abc123" {
		t.Fatalf("rpc id = %q, want abc123", gotID)
	}
	if string(gotBody) != `{"claims_node":"someone-else","ok":true}` {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestRPCRequiresClientCert(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathRPC+"x", "application/octet-stream", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestRPCBodyOverOneMiBRejected(t *testing.T) {
	hub := NewHub(nil)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Hub = hub })
	_, c := f.join(t, nil)
	big := bytes.Repeat([]byte("x"), (1<<20)+10)
	resp, err := c.Post(f.srv.URL+PathRPC+"big", "application/octet-stream", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		t.Fatalf("oversized rpc body accepted: status %d", resp.StatusCode)
	}
}

// TestStreamNegotiatesHTTP2 asserts the stream request itself arrives as
// HTTP/2 (the shipper's transport sets ForceAttemptHTTP2, and the master's
// production TLS config does not opt out of it).
func TestStreamNegotiatesHTTP2(t *testing.T) {
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, err := OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	toks, err := OpenTokens(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	sink := newFakeSink()
	hub := NewHub(nil)
	m := NewMaster(MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: sink, Hub: hub})

	var mu sync.Mutex
	var proto int
	checker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathStream {
			mu.Lock()
			proto = r.ProtoMajor
			mu.Unlock()
		}
		m.Handler().ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(checker)
	srv.TLS = ServerTLS(leaf, ca.Cert)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	f := &masterFixture{ca: ca, reg: reg, toks: toks, sink: sink, srv: srv, pin: SPKIPin(ca.Cert)}
	_, id, _ := startShipperWithFrames(t, f, func(Frame) {}, false)
	waitFor(t, "connected", func() bool { return hub.Connected(id) })

	mu.Lock()
	defer mu.Unlock()
	if proto != 2 {
		t.Fatalf("stream negotiated HTTP/%d, want HTTP/2", proto)
	}
}
