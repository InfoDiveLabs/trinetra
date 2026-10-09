package trinetra

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// --- test harness: a real TLS master with a Hub, real joined children -----
//
// rpcRegistry.Call needs hub.Connected(nodeID) to be genuinely true, which
// only happens over a real stream connection (Hub.connect is unexported to
// package fleet) -- so, like fleet_e2e_test.go, this drives a real
// httptest.Server + fleet.Master + fleet.Join + fleet.Shipper rather than
// faking the hub.

type rpcTestMaster struct {
	t         *testing.T
	ca        *fleet.CA
	leaf      tls.Certificate
	toks      *fleet.TokenStore
	hub       *fleet.Hub
	reg       *rpcRegistry
	sink      *replicaSink
	incidents *incidentStore
	srv       *httptest.Server
}

func newRPCTestMaster(t *testing.T) *rpcTestMaster {
	t.Helper()
	dir := t.TempDir()
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "rpc-test", time.Now()); err != nil {
		t.Fatal(err)
	}
	pki := fleetPKIDir(dir)
	ca, err := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	fleetReg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	toks, err := fleet.OpenTokens(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hub := fleet.NewHub(nil)
	rpcReg := newRPCRegistry(time.Now, t.Logf)
	hub.OnRPCResult(rpcReg.Deliver)
	// sink is wired exactly like startMaster (fleet_daemon.go) wires it in
	// production: hub/rpc/incidents set directly on the field after
	// construction, so NodeAPI's replicaAPI can reach all three.
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	sink.hub = hub
	sink.rpc = rpcReg
	sink.incidents = incidents
	fm := fleet.NewMaster(fleet.MasterConfig{CA: ca, Leaf: leaf, Registry: fleetReg, Tokens: toks, Sink: sink, Hub: hub, Logf: t.Logf})
	srv := httptest.NewUnstartedServer(fm.Handler())
	srv.TLS = fleet.ServerTLS(leaf, ca.Cert)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &rpcTestMaster{t: t, ca: ca, leaf: leaf, toks: toks, hub: hub, reg: rpcReg, sink: sink, incidents: incidents, srv: srv}
}

// connect joins a fresh child node named name, starts its shipper with
// onFrame as its stream handler, and waits until the master's hub sees it
// connected. The returned stop func cancels the shipper and waits for it to
// exit.
func (m *rpcTestMaster) connect(name string, onFrame func(fleet.Frame)) (nodeID string, sh *fleet.Shipper, stop func()) {
	t := m.t
	t.Helper()
	cdir := t.TempDir()
	plain, _, err := m.toks.Create(time.Hour, 1, nil, "rpc-test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code := fleet.EncodeJoin(fleet.JoinInfo{URL: m.srv.URL, Token: plain, Pin: fleet.SPKIPin(m.ca.Cert)})
	res, err := fleet.Join(context.Background(), code, name, "v-test", nil, filepath.Join(cdir, "fleet-child"))
	if err != nil {
		t.Fatal(err)
	}
	ident, err := fleet.LoadIdentity(filepath.Join(cdir, "fleet-child"))
	if err != nil {
		t.Fatal(err)
	}
	ob, err := fleet.OpenOutbox(filepath.Join(cdir, "outbox"), 0)
	if err != nil {
		t.Fatal(err)
	}
	sh = fleet.NewShipper(fleet.ShipperConfig{
		MasterURL: m.srv.URL, Pin: fleet.SPKIPin(m.ca.Cert), Identity: ident, Outbox: ob,
		Live:      func() (fleet.LiveUpdate, error) { return fleet.LiveUpdate{}, nil },
		LiveEvery: time.Hour, Logf: t.Logf,
		OnFrame: onFrame,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sh.Run(ctx) }()
	nodeID = res.NodeID
	waitUntil(t, "node "+name+" connected", func() bool { return m.hub.Connected(nodeID) })
	return nodeID, sh, func() {
		cancel()
		<-done
		_ = ob.Close()
	}
}

// waitUntil polls cond every 10ms for up to 5s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// withShortRPCTimeout lowers rpcCallTimeout for the duration of one test
// (see its own doc comment: a package var precisely so tests can do this),
// restoring it on cleanup.
func withShortRPCTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := rpcCallTimeout
	rpcCallTimeout = d
	t.Cleanup(func() { rpcCallTimeout = old })
}

// fakeLogsAPI is a minimal core.API stub (mirrors fleetCLIFakeAPI) whose
// ContainerLogs is configurable, for exercising handleRPCFrame's child-side
// dispatch end to end.
type fakeLogsAPI struct {
	fleetCLIFakeAPI
	out string
	err error
}

func (f fakeLogsAPI) ContainerLogs(string, int) (string, error) { return f.out, f.err }

// rpcFrame decodes f's rpcFrameData, failing the test if f isn't an "rpc"
// frame with a well-formed payload.
func rpcFrame(t *testing.T, f fleet.Frame) rpcFrameData {
	t.Helper()
	if f.Type != "rpc" {
		t.Fatalf("frame type = %q, want rpc", f.Type)
	}
	var req rpcFrameData
	if err := json.Unmarshal(f.Data, &req); err != nil {
		t.Fatalf("bad rpc frame: %v", err)
	}
	return req
}

// --- rpcRegistry.Call / Deliver --------------------------------------------

// TestRPCRegistryCallHappyPath exercises the full round trip: the master
// pushes an "rpc" frame, a real child dispatches it via handleRPCFrame
// (goroutine, core.API, truncation) and posts the result back over HTTP,
// and Call returns it.
func TestRPCRegistryCallHappyPath(t *testing.T) {
	m := newRPCTestMaster(t)
	var sh *fleet.Shipper
	self := fakeLogsAPI{out: "hello from web-1\n"}
	nodeID, childSh, stop := m.connect("web-1", func(f fleet.Frame) {
		handleRPCFrame(self, sh, t.Logf, f)
	})
	sh = childSh
	defer stop()
	defer drainRPCSemForTest(t) // see its doc comment: required by every test exercising the real handleRPCFrame

	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 50})
	res, err := m.reg.Call(m.hub, nodeID, "container_logs", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.OK || res.Output != "hello from web-1\n" {
		t.Fatalf("res = %+v", res)
	}
}

// TestRPCRegistryCallNodeNotConnected pins the exact wording (task-9
// ruling) for a node with no open stream connection -- no live server is
// even needed for this one.
func TestRPCRegistryCallNodeNotConnected(t *testing.T) {
	hub := fleet.NewHub(nil)
	reg := newRPCRegistry(time.Now, t.Logf)
	_, err := reg.Call(hub, "some-node-id", "container_logs", nil)
	if !errors.Is(err, errNodeNotConnected) {
		t.Fatalf("err = %v, want errNodeNotConnected", err)
	}
	if err.Error() != "node is not connected" {
		t.Fatalf("err text = %q", err.Error())
	}
}

// TestRPCRegistryCallTimeout: a connected node that never answers times out
// after rpcCallTimeout with the exact wording the ruling specifies.
func TestRPCRegistryCallTimeout(t *testing.T) {
	withShortRPCTimeout(t, 100*time.Millisecond)
	m := newRPCTestMaster(t)
	nodeID, _, stop := m.connect("silent", func(fleet.Frame) {
		// never answers
	})
	defer stop()

	_, err := m.reg.Call(m.hub, nodeID, "container_logs", nil)
	if !errors.Is(err, errRPCTimeout) {
		t.Fatalf("err = %v, want errRPCTimeout", err)
	}
	if err.Error() != "node did not answer in 10s" {
		t.Fatalf("err text = %q, want the fixed 10s wording regardless of the test's shortened timeout", err.Error())
	}
}

// TestRPCRegistryRejectsResultFromWrongNode is the id-spoofing guard
// (Hub.OnRPCResult's own doc comment): a result posted by node B for an id
// the registry actually sent to node A must be rejected -- logged, never
// delivered -- and the pending call must still be resolvable by the RIGHT
// node afterward.
func TestRPCRegistryRejectsResultFromWrongNode(t *testing.T) {
	m := newRPCTestMaster(t)
	var rejectLogs []string
	m.reg.logf = func(format string, args ...any) { rejectLogs = append(rejectLogs, fmt.Sprintf(format, args...)) }

	idCh := make(chan string, 1)
	nodeA, shA, stopA := m.connect("node-a", func(f fleet.Frame) {
		idCh <- rpcFrame(t, f).ID
	})
	defer stopA()
	_, shB, stopB := m.connect("node-b", func(fleet.Frame) {})
	defer stopB()

	resCh := make(chan rpcResultData, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := m.reg.Call(m.hub, nodeA, "container_logs", nil)
		if err != nil {
			errCh <- err
			return
		}
		resCh <- res
	}()

	var id string
	select {
	case id = <-idCh:
	case <-time.After(2 * time.Second):
		t.Fatal("node A never received the rpc frame")
	}

	// Node B posts a result for an id that was sent to node A, not to it.
	if err := shB.PostRPCResult(context.Background(), id, []byte(`{"ok":true,"output":"stolen"}`)); err != nil {
		t.Fatalf("post from wrong node: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let the rejection (or a wrongful delivery) land
	select {
	case res := <-resCh:
		t.Fatalf("Call returned a result from the wrong node: %+v", res)
	case err := <-errCh:
		t.Fatalf("Call errored before the real node ever answered: %v", err)
	default:
	}
	if len(rejectLogs) == 0 {
		t.Fatal("expected the wrong-node result to be logged as rejected")
	}

	// Now the actual target node answers, and the ORIGINAL Call must still
	// resolve to it -- proof the spoofed post never consumed the pending
	// entry.
	if err := shA.PostRPCResult(context.Background(), id, []byte(`{"ok":true,"output":"real"}`)); err != nil {
		t.Fatalf("post from the real node: %v", err)
	}
	select {
	case res := <-resCh:
		if res.Output != "real" {
			t.Fatalf("output = %q, want the real node's answer", res.Output)
		}
	case err := <-errCh:
		t.Fatalf("Call errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the real node's result")
	}
}

// TestRPCRegistryDeliverRejectsUnknownID: a result for an id nobody ever
// registered (no Call was ever made for it) is rejected and logged, not
// delivered to anything, and does not panic.
func TestRPCRegistryDeliverRejectsUnknownID(t *testing.T) {
	var logs []string
	reg := newRPCRegistry(time.Now, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	reg.Deliver("some-node", "deadbeefdeadbeef", []byte(`{"ok":true}`))
	if len(logs) == 0 {
		t.Fatal("expected a rejection log line for an unknown id")
	}
}

// TestRPCRegistryRejectionLogsRateLimitedPerNode pins the round-1 fix
// (mirrors fleet.Hub.logDrop): a node spamming rejected results is logged
// at most once per second, not once per rejection -- but a DIFFERENT node
// gets its own independent line, exactly like logDrop's per-node map.
func TestRPCRegistryRejectionLogsRateLimitedPerNode(t *testing.T) {
	fakeNow := time.Unix(1_700_000_000, 0)
	var logs []string
	reg := newRPCRegistry(func() time.Time { return fakeNow }, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	})

	for i := 0; i < 5; i++ {
		reg.Deliver("node-a", "deadbeefdeadbeef", []byte(`{"ok":true}`))
	}
	if len(logs) != 1 {
		t.Fatalf("5 rejections within the same second logged %d lines, want 1", len(logs))
	}

	// A different node is never suppressed by node-a's rate limit.
	reg.Deliver("node-b", "deadbeefdeadbeef", []byte(`{"ok":true}`))
	if len(logs) != 2 {
		t.Fatalf("a different node's rejection was suppressed: logs = %+v", logs)
	}

	// Once a full second has passed, node-a logs again.
	fakeNow = fakeNow.Add(time.Second)
	reg.Deliver("node-a", "deadbeefdeadbeef", []byte(`{"ok":true}`))
	if len(logs) != 3 {
		t.Fatalf("node-a's rejection after 1s was suppressed: logs = %+v", logs)
	}
}

// TestRPCRegistryDeliverRejectsDuplicate: once a result has been delivered
// for an id, a second POST for the same id (a retry, or an attempted
// replay) is rejected -- the entry is single-use.
func TestRPCRegistryDeliverRejectsDuplicate(t *testing.T) {
	m := newRPCTestMaster(t)
	var sh *fleet.Shipper
	var gotID string
	self := fakeLogsAPI{out: "first"}
	nodeID, childSh, stop := m.connect("dup", func(f fleet.Frame) {
		gotID = rpcFrame(t, f).ID
		handleRPCFrame(self, sh, t.Logf, f)
	})
	sh = childSh
	defer stop()
	defer drainRPCSemForTest(t) // see its doc comment: required by every test exercising the real handleRPCFrame

	var rejectLogs []string
	m.reg.logf = func(format string, args ...any) { rejectLogs = append(rejectLogs, fmt.Sprintf(format, args...)) }

	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 10})
	res, err := m.reg.Call(m.hub, nodeID, "container_logs", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != "first" {
		t.Fatalf("res = %+v", res)
	}
	if gotID == "" {
		t.Fatal("never observed the rpc frame's id")
	}

	// The id is now removed (delivered once). A second POST for it must be
	// rejected, not delivered anywhere (nothing is even waiting on it any
	// more -- Call already returned above).
	if err := childSh.PostRPCResult(context.Background(), gotID, []byte(`{"ok":true,"output":"second"}`)); err != nil {
		t.Fatalf("second post: %v", err)
	}
	waitUntil(t, "duplicate rejection logged", func() bool { return len(rejectLogs) > 0 })
}

// TestRPCRegistryCapPerNode pins the 32-pending-per-node cap: with the cap
// already reached, a new Call for that node is refused immediately (no
// wait for the actual RPC timeout), with the exact wording the ruling
// specifies. Fake entries are seeded directly (white-box) rather than via
// 32 real in-flight Calls, so the test is deterministic and leaves nothing
// to clean up.
func TestRPCRegistryCapPerNode(t *testing.T) {
	m := newRPCTestMaster(t)
	nodeID, _, stop := m.connect("busy", func(fleet.Frame) {})
	defer stop()

	m.reg.mu.Lock()
	for i := 0; i < rpcMaxPendingPerNode; i++ {
		id := fmt.Sprintf("fake%02d", i)
		m.reg.pending[id] = &pendingCall{node: nodeID, created: time.Now(), ch: make(chan rpcResultData, 1)}
	}
	m.reg.mu.Unlock()

	_, err := m.reg.Call(m.hub, nodeID, "container_logs", nil)
	if !errors.Is(err, errTooManyPendingRPCs) {
		t.Fatalf("err = %v, want errTooManyPendingRPCs", err)
	}
	if err.Error() != "too many pending requests for this node" {
		t.Fatalf("err text = %q", err.Error())
	}
}

// TestRPCRegistrySweepDropsOldEntries pins the 60s-past-expiry sweep
// an entry sitting well past rpcCallTimeout+rpcSweepAfter
// is dropped the next time sweepLocked runs (triggered here by another
// Call for the same node), freeing its slot even though nothing ever
// resolved or timed it out through the normal Call path.
func TestRPCRegistrySweepDropsOldEntries(t *testing.T) {
	fakeNow := time.Unix(1_700_000_000, 0)
	reg := newRPCRegistry(func() time.Time { return fakeNow }, nil)
	reg.mu.Lock()
	reg.pending["stale"] = &pendingCall{node: "n1", created: fakeNow.Add(-(rpcCallTimeout + rpcSweepAfter + time.Second)), ch: make(chan rpcResultData, 1)}
	reg.mu.Unlock()

	reg.mu.Lock()
	reg.sweepLocked()
	_, stillThere := reg.pending["stale"]
	reg.mu.Unlock()
	if stillThere {
		t.Fatal("stale entry survived a sweep")
	}
}

// --- child-side dispatch (fleet_rpc_child.go) -------------------------------

// TestDispatchContainerLogsTruncatesFromStart pins the exact truncation
// contract: output over rpcMaxOutputBytes is capped,
// prefixed with a marker, and keeps the END of the original (the most
// recent log lines), not the start.
func TestDispatchContainerLogsTruncatesFromStart(t *testing.T) {
	big := strings.Repeat("a", rpcMaxOutputBytes) + "TAIL-MARKER"
	self := fakeLogsAPI{out: big}
	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 10})
	res := dispatchContainerLogs(self, args)
	if !res.OK {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Output) > rpcMaxOutputBytes {
		t.Fatalf("output len = %d, want <= %d", len(res.Output), rpcMaxOutputBytes)
	}
	if !strings.HasPrefix(res.Output, rpcTruncatedMarker) {
		t.Fatalf("output does not start with the truncation marker: %q...", res.Output[:60])
	}
	if !strings.HasSuffix(res.Output, "TAIL-MARKER") {
		t.Fatal("truncation dropped the END of the output; it must drop the START instead")
	}
}

// TestDispatchContainerLogsBoundsLines pins the "at most 2000 lines" bound
// whatever the master asked for, self.ContainerLogs never
// sees more than rpcMaxLines.
func TestDispatchContainerLogsBoundsLines(t *testing.T) {
	var gotLines int
	self := fakeContainerLogsCapture{out: "ok", capture: &gotLines}
	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 999999})
	res := dispatchContainerLogs(self, args)
	if !res.OK || res.Output != "ok" {
		t.Fatalf("res = %+v", res)
	}
	if gotLines != rpcMaxLines {
		t.Fatalf("lines passed to ContainerLogs = %d, want %d (capped)", gotLines, rpcMaxLines)
	}
}

type fakeContainerLogsCapture struct {
	fleetCLIFakeAPI
	out     string
	capture *int
}

func (f fakeContainerLogsCapture) ContainerLogs(name string, lines int) (string, error) {
	*f.capture = lines
	return f.out, nil
}

// TestDispatchContainerLogsPassesThroughChildError: the child's own
// ContainerLogs error is passed through verbatim (task-9 ruling: "the
// child's own error, passed through").
func TestDispatchContainerLogsPassesThroughChildError(t *testing.T) {
	self := fakeLogsAPI{err: errors.New("docker is not available on this host")}
	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 10})
	res := dispatchContainerLogs(self, args)
	if res.OK || res.Error != "docker is not available on this host" {
		t.Fatalf("res = %+v", res)
	}
}

// TestDispatchRPCUnknownMethod pins the exact wording for a
// method this child doesn't implement.
func TestDispatchRPCUnknownMethod(t *testing.T) {
	res := dispatchRPC(fakeLogsAPI{}, rpcFrameData{ID: "x", Method: "reboot"})
	if res.OK || res.Error != "unknown method" {
		t.Fatalf("res = %+v", res)
	}
}

// TestHandleRPCFrameRunsOffTheReadLoop pins the "own goroutine, never on the
// read loop" requirement, end to end over a real connected
// child: a first rpc frame whose self.ContainerLogs blocks must not stall
// the child's stream read loop -- a second rpc frame sent to the very same
// node must still be read and answered while the first is still stuck.
func TestHandleRPCFrameRunsOffTheReadLoop(t *testing.T) {
	m := newRPCTestMaster(t)
	release := make(chan struct{})
	blocker := blockingLogsAPI{release: release}
	fast := fakeLogsAPI{out: "second"}

	var sh *fleet.Shipper
	nodeID, childSh, stop := m.connect("blocker", func(f fleet.Frame) {
		req := rpcFrame(t, f)
		var args rpcContainerLogsArgs
		_ = json.Unmarshal(req.Args, &args)
		if args.Name == "blocker" {
			handleRPCFrame(blocker, sh, t.Logf, f)
		} else {
			handleRPCFrame(fast, sh, t.Logf, f)
		}
	})
	sh = childSh
	defer stop()
	// Ensure the "blocker" worker's own rpcSem release has genuinely
	// happened before this test returns (registered after defer stop() so
	// it runs FIRST, while the connection is still up): otherwise a
	// straggling handleRPCFrame goroutine from this test can race the next
	// test's read of rpcSem (see TestHandleRPCFrameConcurrencyBounded's own
	// use of drainRPCSemForTest for the same reason, and the round-1 review
	// fix this pins).
	defer drainRPCSemForTest(t)

	firstArgs, _ := json.Marshal(rpcContainerLogsArgs{Name: "blocker", Lines: 1})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = m.reg.Call(m.hub, nodeID, "container_logs", firstArgs)
	}()
	// Give the first call time to reach the child and start blocking inside
	// ContainerLogs before firing the second.
	time.Sleep(150 * time.Millisecond)

	secondArgs, _ := json.Marshal(rpcContainerLogsArgs{Name: "fast", Lines: 1})
	res, err := m.reg.Call(m.hub, nodeID, "container_logs", secondArgs)
	close(release)
	<-firstDone
	if err != nil {
		t.Fatalf("second Call: %v (the first call's dispatch must not block the read loop)", err)
	}
	if !res.OK || res.Output != "second" {
		t.Fatalf("res = %+v", res)
	}
}

type blockingLogsAPI struct {
	fleetCLIFakeAPI
	release chan struct{}
}

func (b blockingLogsAPI) ContainerLogs(string, int) (string, error) {
	<-b.release
	return "", nil
}

// drainRPCSemForTest blocks until every rpcSem slot currently held by an
// in-flight (or straggling) handleRPCFrame worker has actually been
// released, then leaves rpcSem empty again. It must be called (directly or
// via defer, registered early enough to run before the test's connection
// goes down) by any test whose self.ContainerLogs runs through the real
// handleRPCFrame, before that test returns -- otherwise a straggling
// worker goroutine (its result already reached the master, but its own
// deferred `<-rpcSem` hasn't executed yet -- these are NOT the same
// instant, see handleRPCFrame's release-after-PostRPCResult ordering) can
// still be reading the rpcSem variable when a LATER test reassigns it
// (withFreshRPCSem), or just leave a stale held slot for a later test that
// assumes rpcSem starts empty.
//
// Sending rpcMaxConcurrent more values into rpcSem is a genuine
// synchronization point, unlike polling len(rpcSem): each send can only
// complete once a slot is actually freed by a worker's own receive, and
// the Go memory model guarantees that receive happens-before this send
// completes (the standard channel-as-semaphore guarantee) -- len() reads
// the buffer's current count but establishes no such happens-before edge,
// so a race detector can (and, empirically, sometimes does) still flag a
// concurrent access it can't prove is safe.
func drainRPCSemForTest(t *testing.T) {
	t.Helper()
	for i := 0; i < rpcMaxConcurrent; i++ {
		select {
		case rpcSem <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatal("a handleRPCFrame worker never released its rpcSem slot")
		}
	}
	for i := 0; i < rpcMaxConcurrent; i++ {
		<-rpcSem
	}
}

// withFreshRPCSem swaps in a brand-new rpcSem for the duration of one test
// (restored via t.Cleanup), exactly like withShortRPCTimeout does for
// rpcCallTimeout: a concurrency test needs a semaphore starting empty and
// entirely its own, not one still holding slots from -- or being raced by
// -- whatever ran before it. The test itself is still responsible for
// calling drainRPCSemForTest before it returns, so the swap-back in
// t.Cleanup is never racing a straggler from THIS test either.
func withFreshRPCSem(t *testing.T) {
	t.Helper()
	old := rpcSem
	rpcSem = make(chan struct{}, rpcMaxConcurrent)
	t.Cleanup(func() { rpcSem = old })
}

// trackingBlockingLogsAPI blocks every call on release, counting how many
// are in ContainerLogs at once (current) and the high-water mark (peak),
// guarded by mu.
type trackingBlockingLogsAPI struct {
	fleetCLIFakeAPI
	release chan struct{}

	mu      sync.Mutex
	current int
	peak    int
}

func (b *trackingBlockingLogsAPI) ContainerLogs(string, int) (string, error) {
	b.mu.Lock()
	b.current++
	if b.current > b.peak {
		b.peak = b.current
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.current--
	b.mu.Unlock()
	return "ok", nil
}

// TestHandleRPCFrameConcurrencyBounded is the round-1 review fix, IMPORTANT
// 2: 20 rapid rpc frames for the same node never run more than
// rpcMaxConcurrent (8) at once, and every frame beyond that cap is refused
// immediately with {"ok":false,"error":"node busy"} instead of being queued
// or run anyway.
//
// This drives the full real master<->child round trip (like
// TestRPCRegistryCallHappyPath) rather than calling handleRPCFrame
// directly, specifically so the "extras get node busy" half of the
// assertion is checked exactly the way a real caller (replicaAPI.
// ContainerLogs, via rpcRegistry.Call) actually observes it: as the
// rpcResultData a Call returns, not as an internal implementation detail.
func TestHandleRPCFrameConcurrencyBounded(t *testing.T) {
	withFreshRPCSem(t)
	m := newRPCTestMaster(t)
	release := make(chan struct{})
	self := &trackingBlockingLogsAPI{release: release}

	var sh *fleet.Shipper
	nodeID, childSh, stop := m.connect("busy-node", func(f fleet.Frame) {
		handleRPCFrame(self, sh, t.Logf, f)
	})
	sh = childSh
	defer stop()

	const n = 20
	type outcome struct {
		res rpcResultData
		err error
	}
	results := make(chan outcome, n)
	args, _ := json.Marshal(rpcContainerLogsArgs{Name: "web", Lines: 1})
	for i := 0; i < n; i++ {
		go func() {
			res, err := m.reg.Call(m.hub, nodeID, "container_logs", args)
			results <- outcome{res, err}
		}()
	}

	// The busy rejections resolve near-instantly (no semaphore wait, no
	// blocking work); the rpcMaxConcurrent genuinely-running ones stay
	// blocked on release. Collect exactly the busy ones first.
	const wantBusy = n - rpcMaxConcurrent
	busy := 0
	for busy < wantBusy {
		select {
		case o := <-results:
			if o.err != nil {
				t.Fatalf("Call error: %v", o.err)
			}
			if o.res.OK || o.res.Error != "node busy" {
				t.Fatalf("expected a busy rejection, got %+v", o.res)
			}
			busy++
		case <-time.After(5 * time.Second):
			t.Fatalf("only got %d/%d busy rejections", busy, wantBusy)
		}
	}

	self.mu.Lock()
	cur, peak := self.current, self.peak
	self.mu.Unlock()
	if cur != rpcMaxConcurrent {
		t.Fatalf("currently running = %d, want exactly %d (the busy slots must be genuinely occupied, not merely counted)", cur, rpcMaxConcurrent)
	}
	if peak > rpcMaxConcurrent {
		t.Fatalf("peak concurrency = %d, want <= %d", peak, rpcMaxConcurrent)
	}

	close(release)
	for i := 0; i < rpcMaxConcurrent; i++ {
		select {
		case o := <-results:
			if o.err != nil {
				t.Fatalf("Call error: %v", o.err)
			}
			if !o.res.OK || o.res.Output != "ok" {
				t.Fatalf("expected a real ok result, got %+v", o.res)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for the real (non-busy) results")
		}
	}
	if peak > rpcMaxConcurrent {
		t.Fatalf("peak concurrency (final) = %d, want <= %d", peak, rpcMaxConcurrent)
	}
	// Collecting all rpcMaxConcurrent results does not by itself guarantee
	// every worker goroutine has finished running yet (its own rpcSem
	// release happens strictly after its PostRPCResult call returns, which
	// is strictly after the master already received and delivered the
	// result above) -- see drainRPCSemForTest's doc comment for why this
	// matters before withFreshRPCSem's t.Cleanup swaps rpcSem back.
	drainRPCSemForTest(t)
}
