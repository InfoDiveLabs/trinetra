package fleet

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func joinCode(f *masterFixture, t *testing.T, uses int) string {
	plain, _, err := f.toks.Create(time.Hour, uses, []string{"lab"}, "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return EncodeJoin(JoinInfo{URL: f.srv.URL, Token: plain, Pin: f.pin})
}

func TestJoinWritesIdentityAndRejoinKeepsID(t *testing.T) {
	f := newMasterFixture(t)
	dir := filepath.Join(t.TempDir(), "fleet-child")
	res, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.NodeID == "" || res.MasterURL != f.srv.URL || res.Pin != f.pin {
		t.Fatalf("result = %+v", res)
	}
	assertMode(t, filepath.Join(dir, "node.key"), 0o600)
	id, err := LoadIdentity(dir)
	if err != nil || id.NodeID() != res.NodeID {
		t.Fatalf("identity %v err %v", id, err)
	}
	res2, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res2.NodeID != res.NodeID {
		t.Fatalf("rejoin changed id %s -> %s", res.NodeID, res2.NodeID)
	}
}

func TestJoinWithWrongPinSendsNothing(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 1, nil, "t", time.Now())
	other, _ := NewCA("other", time.Now())
	code := EncodeJoin(JoinInfo{URL: f.srv.URL, Token: plain, Pin: SPKIPin(other.Cert)})
	if _, err := Join(context.Background(), code, "box", "v1", nil, t.TempDir()); err == nil {
		t.Fatal("join succeeded against wrong pin")
	}
	if len(f.toks.List(time.Now())) != 1 {
		t.Fatal("token was consumed despite pin mismatch")
	}
}

func startShipper(t *testing.T, f *masterFixture, ob *Outbox, gaps GapFiller) (*Shipper, context.CancelFunc) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet-child")
	if _, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir); err != nil {
		t.Fatal(err)
	}
	id, _ := LoadIdentity(dir)
	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob, Gaps: gaps,
		Live:      func() (LiveUpdate, error) { return LiveUpdate{Version: "v1", Snapshot: json.RawMessage(`{}`)}, nil },
		LiveEvery: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	t.Cleanup(cancel)
	return sh, cancel
}

// startShipperWithFastBackoff is startShipper with the data-loop retry
// backoff shortened to milliseconds, for tests that deliberately trigger
// several retries (e.g. gap-fill failures) and would otherwise wait out
// backoffDelay's real 1s-60s jittered wall-clock sleeps.
func startShipperWithFastBackoff(t *testing.T, f *masterFixture, ob *Outbox, gaps GapFiller) (*Shipper, context.CancelFunc) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet-child")
	if _, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir); err != nil {
		t.Fatal(err)
	}
	id, _ := LoadIdentity(dir)
	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob, Gaps: gaps,
		Live:      func() (LiveUpdate, error) { return LiveUpdate{Version: "v1", Snapshot: json.RawMessage(`{}`)}, nil },
		LiveEvery: 50 * time.Millisecond,
	})
	sh.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)
	t.Cleanup(cancel)
	return sh, cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestShipperDrainsOutboxAndSendsLive(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	appendN(t, ob, 1, 20)
	sh, _ := startShipper(t, f, ob, nil)
	waitFor(t, "outbox drained", func() bool { return ob.Acked() == 20 })
	waitFor(t, "live update", func() bool {
		f.sink.mu.Lock()
		defer f.sink.mu.Unlock()
		return len(f.sink.live) == 1
	})
	if sh.Status().State != "linked" {
		t.Fatalf("state = %q", sh.Status().State)
	}
	appendN(t, ob, 21, 5) // new data after idle is picked up via Notify
	waitFor(t, "second drain", func() bool { return ob.Acked() == 25 })
}

// TestShipperPriorityLaneShipsAlertBeforeBacklog: a large samples backlog
// (10k records, more than one Ingest batch's worth at MaxBatchRecords) plus
// one freshly-fired alert. The alert must reach the fake master via
// Backfill before the backlog's first Ingest/Apply call -- proven by call
// order at the sink, not by timing -- and the backlog must still fully
// apply afterwards: nothing the priority lane jumped ahead of is lost.
//
// Because the priority lane never Acks (Backfill carries no seq
// bookkeeping) it keeps re-offering the same still-unacked alert on every
// shipOnce call until the backlog's own Ingest sweep naturally reaches that
// seq and Acks it for real -- so with a backlog spanning multiple Ingest
// batches, the alert is backfilled more than once here. That is the
// intended "re-sent and deduped" behaviour (a real master's replica alert
// guard, exercised elsewhere, is what makes the extra sends free); what
// must hold is that every one of those backfills happens before the apply
// call it precedes, that only the alert's own seq is ever backfilled, and
// that the backlog still converges to fully applied.
func TestShipperPriorityLaneShipsAlertBeforeBacklog(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	appendN(t, ob, 1, 10000)
	alertSeq, err := ob.Append(KindAlert, 999, []byte(`{"key":"cpu","fired_at":999}`))
	if err != nil {
		t.Fatal(err)
	}
	if alertSeq != 10001 {
		t.Fatalf("test setup: alert seq = %d, want 10001", alertSeq)
	}

	startShipperWithFastBackoff(t, f, ob, nil)

	waitFor(t, "backlog fully applied", func() bool { return ob.Acked() >= alertSeq })

	order := f.sink.Order()
	if len(order) == 0 {
		t.Fatal("no calls reached the fake master")
	}
	wantAlertBackfill := fmt.Sprintf("backfill:%d", alertSeq)
	if order[0] != wantAlertBackfill {
		t.Fatalf("order[0] = %q, want %q (the priority lane first, before any backlog apply)", order[0], wantAlertBackfill)
	}
	for _, call := range order {
		if strings.HasPrefix(call, "backfill:") && call != wantAlertBackfill {
			t.Fatalf("order = %v: a backfill for something other than the alert (seq %d)", order, alertSeq)
		}
	}

	nodeID := nodeIDFromOutbox(t, f, ob)
	backfilled := f.sink.BackfillFor(nodeID)
	if len(backfilled) == 0 {
		t.Fatal("nothing was backfilled")
	}
	for _, r := range backfilled {
		if r.Seq != alertSeq || r.Kind != KindAlert {
			t.Fatalf("backfilled = %+v, want only the alert record (seq %d)", backfilled, alertSeq)
		}
	}

	// Nothing lost: every backlog record plus the alert was eventually
	// applied for real via the ordinary Ingest path (the fake sink's Apply
	// just counts seqs; production content-level dedup of the alert's
	// repeat arrival there is package trinetra's replicaNode.apply, and
	// TestIngestIsIdempotentAndOrdered covers Ingest's own seq dedup).
	if applied := f.sink.AppliedFor(nodeID); applied != alertSeq {
		t.Fatalf("applied = %d, want %d", applied, alertSeq)
	}
	if got := len(f.sink.AppliedRecsFor(nodeID)); got != int(alertSeq) {
		t.Fatalf("applied %d records total, want %d (10000 samples + the alert)", got, alertSeq)
	}
}

// nodeIDFromOutbox returns the sole node id the fixture's sink has heard
// from, for tests where the shipper (and so the node id) was started
// directly rather than through startShipper's return value.
func nodeIDFromOutbox(t *testing.T, f *masterFixture, ob *Outbox) string {
	t.Helper()
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	for id := range f.sink.applied {
		return id
	}
	for id := range f.sink.backfill {
		return id
	}
	t.Fatal("no node has contacted the fake master yet")
	return ""
}

type fakeGaps struct{ calls int }

func (g *fakeGaps) Fill(gap Gap) ([]Record, error) {
	g.calls++
	return []Record{{Kind: KindAlert, TS: gap.MinTS, Data: json.RawMessage(`{}`)}}, nil
}

func TestShipperRepairsGapsBeforeOutbox(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := openOutbox(t.TempDir(), 3000, 1000)
	appendN(t, ob, 1, 200) // forces a gap
	if len(ob.Gaps()) == 0 {
		t.Fatal("setup: no gap")
	}
	g := &fakeGaps{}
	startShipper(t, f, ob, g)
	waitFor(t, "drained", func() bool { return ob.Acked() == 200 && len(ob.Gaps()) == 0 })
	if g.calls != 1 {
		t.Fatalf("gap filler calls = %d", g.calls)
	}
}

// flakyGaps fails Fill some number of times (or forever, if always is set)
// before succeeding, so tests can exercise the shipper's transient-failure
// retry and abandon-after-N-attempts behaviour.
type flakyGaps struct {
	mu     sync.Mutex
	fails  int
	always bool
	calls  int
}

func (g *flakyGaps) Fill(gap Gap) ([]Record, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	if g.always || g.fails > 0 {
		if !g.always {
			g.fails--
		}
		return nil, errors.New("fleet: simulated local rebuild failure")
	}
	return []Record{{Kind: KindAlert, TS: gap.MinTS, Data: json.RawMessage(`{}`)}}, nil
}

func (g *flakyGaps) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func TestShipperRetriesGapFillOnTransientError(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := openOutbox(t.TempDir(), 3000, 1000)
	appendN(t, ob, 1, 200) // forces a gap
	if len(ob.Gaps()) == 0 {
		t.Fatal("setup: no gap")
	}
	g := &flakyGaps{fails: 2}
	startShipperWithFastBackoff(t, f, ob, g)
	waitFor(t, "drained after transient fill failures", func() bool {
		return ob.Acked() == 200 && len(ob.Gaps()) == 0
	})
	if calls := g.callCount(); calls < 3 {
		t.Fatalf("gap filler calls = %d, want at least 3 (2 failures then a success)", calls)
	}
	f.sink.mu.Lock()
	defer f.sink.mu.Unlock()
	if len(f.sink.backfill) == 0 {
		t.Fatal("no backfill data reached the master; the repaired gap was dropped instead of retried")
	}
}

func TestShipperAbandonsGapAfterRepeatedFillFailures(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := openOutbox(t.TempDir(), 3000, 1000)
	appendN(t, ob, 1, 200) // forces a gap
	if len(ob.Gaps()) == 0 {
		t.Fatal("setup: no gap")
	}
	g := &flakyGaps{always: true}
	startShipperWithFastBackoff(t, f, ob, g)
	waitFor(t, "gap abandoned and outbox still drains", func() bool {
		return ob.Acked() == 200 && len(ob.Gaps()) == 0
	})
	if calls := g.callCount(); calls < gapFillMaxAttempts {
		t.Fatalf("gap filler calls = %d, want at least %d before abandoning", calls, gapFillMaxAttempts)
	}
}

func TestBatchSizeCapsByRecordsAndBytes(t *testing.T) {
	rec := func(n int) Record { return Record{Data: json.RawMessage(make([]byte, n))} }

	if n := batchSize(nil); n != 0 {
		t.Fatalf("empty input: batchSize = %d, want 0", n)
	}
	if n := batchSize([]Record{rec(MaxBatchBytes + 1)}); n != 1 {
		t.Fatalf("oversized single record: batchSize = %d, want 1 (always at least one)", n)
	}
	if n := batchSize([]Record{rec(MaxBatchBytes/2 + 1), rec(MaxBatchBytes/2 + 1), rec(1)}); n != 1 {
		t.Fatalf("byte cap: batchSize = %d, want 1", n)
	}
	many := make([]Record, MaxBatchRecords+5)
	for i := range many {
		many[i] = rec(1)
	}
	if n := batchSize(many); n != MaxBatchRecords {
		t.Fatalf("record cap: batchSize = %d, want %d", n, MaxBatchRecords)
	}
}

func TestShipperStopsWhenRevoked(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	sh, _ := startShipper(t, f, ob, nil)
	waitFor(t, "linked", func() bool { return sh.Status().State == "linked" })
	for _, n := range f.reg.List() {
		f.reg.Update(n.ID, func(n *Node) error { n.Revoked = true; return nil })
	}
	appendN(t, ob, 1, 1)
	waitFor(t, "revoked", func() bool { return sh.Status().State == "revoked" })
	if ob.Acked() != 0 {
		t.Fatal("revoked node's data was acked")
	}
}

func TestShipperRetriesWhileMasterDown(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	appendN(t, ob, 1, 3)
	dir := filepath.Join(t.TempDir(), "fleet-child")
	if _, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir); err != nil {
		t.Fatal(err)
	}
	url := f.srv.URL
	f.srv.Close() // the master goes away after the join
	id, _ := LoadIdentity(dir)
	sh := NewShipper(ShipperConfig{MasterURL: url, Pin: f.pin, Identity: id, Outbox: ob,
		Live: func() (LiveUpdate, error) { return LiveUpdate{}, nil }, LiveEvery: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitFor(t, "retrying", func() bool { return sh.Status().State == "retrying" })
	if ob.Acked() != 0 || sh.Status().LastError == "" {
		t.Fatalf("status %+v acked %d", sh.Status(), ob.Acked())
	}
}

func TestRunReturnsWhenLiveCallbackHangs(t *testing.T) {
	f := newMasterFixture(t)
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	dir := filepath.Join(t.TempDir(), "fleet-child")
	if _, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir); err != nil {
		t.Fatal(err)
	}
	id, _ := LoadIdentity(dir)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) }) // release the one abandoned goroutine

	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob,
		Live: func() (LiveUpdate, error) {
			<-block
			return LiveUpdate{}, nil
		},
		LiveEvery: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()

	// Give liveLoop time to call the hanging Live() at least once.
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return within 3s of ctx cancellation while Live was hanging")
	}
}

// TestShipperRecoversFromOutboxDivergence: the master already applied up to
// seq 100 for this node (e.g. the child's outbox was restored from an old
// backup or deleted) while the child's outbox restarts at 1. The master drops
// seqs 1..3 as already applied, and acks 100. The child must not lose those
// records: they are re-sent via backfill, and newer records get seqs > 100.
func TestShipperRecoversFromOutboxDivergence(t *testing.T) {
	f := newMasterFixture(t)
	dir := filepath.Join(t.TempDir(), "fleet-child")
	res, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	f.sink.mu.Lock()
	f.sink.applied[res.NodeID] = 100
	f.sink.mu.Unlock()
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	appendN(t, ob, 500, 3) // seqs 1..3, ts 500..502
	id, _ := LoadIdentity(dir)
	var logged atomic.Int32
	sh := NewShipper(ShipperConfig{
		MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: ob, Gaps: &fakeGaps{},
		LiveEvery: time.Hour,
		Logf: func(format string, args ...any) {
			if strings.Contains(fmt.Sprintf(format, args...), "diverged") {
				logged.Add(1)
			}
		},
	})
	sh.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sh.Run(ctx)
	waitFor(t, "divergent range backfilled", func() bool {
		f.sink.mu.Lock()
		defer f.sink.mu.Unlock()
		for _, r := range f.sink.backfill[res.NodeID] {
			if r.TS == 500 {
				return true
			}
		}
		return false
	})
	if logged.Load() == 0 {
		t.Fatal("divergence was not logged")
	}
	if seq, _ := ob.Append(KindSamples, 600, []byte(`{}`)); seq <= 100 {
		t.Fatalf("seq after divergence = %d, want > 100", seq)
	}
	waitFor(t, "new record applied", func() bool {
		f.sink.mu.Lock()
		defer f.sink.mu.Unlock()
		return f.sink.applied[res.NodeID] == 101
	})
}

// joinedIdentity joins f and returns the identity dir and node id.
func joinedIdentity(t *testing.T, f *masterFixture) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet-child")
	res, err := Join(context.Background(), joinCode(f, t, 1), "box", "v1", nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res.NodeID
}

// newerPair signs a fresh key for id, as a renewal would.
func newerPair(t *testing.T, f *masterFixture, id string) (keyPEM, certPEM []byte, serial string) {
	t.Helper()
	keyPEM, csrPEM, err := NewKeyAndCSR(id)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, _, err = f.ca.SignClient(csrPEM, id, time.Now().Add(time.Hour), ClientCertLife)
	if err != nil {
		t.Fatal(err)
	}
	return keyPEM, certPEM, serial
}

func loadedSerial(t *testing.T, id *Identity) string {
	t.Helper()
	c, _ := id.Cert()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return CertSerialHex(leaf)
}

func TestRenewRotatesIdentityAndLeavesNoStagingFiles(t *testing.T) {
	f := newMasterFixture(t)
	dir, nodeID := joinedIdentity(t, f)
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	before := loadedSerial(t, id)
	sh := NewShipper(ShipperConfig{MasterURL: f.srv.URL, Pin: f.pin, Identity: id, Outbox: nil})
	if err := id.Renew(context.Background(), sh.client, f.srv.URL); err != nil {
		t.Fatal(err)
	}
	after := loadedSerial(t, id)
	if after == before {
		t.Fatal("renew kept the old certificate")
	}
	if n, _ := f.reg.Get(nodeID); n.CertSerial != after {
		t.Fatalf("registry serial %s, identity serial %s", n.CertSerial, after)
	}
	for _, name := range []string{"node.key.new", "node.crt.new"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind (err=%v)", name, err)
		}
	}
	reloaded, err := LoadIdentity(dir)
	if err != nil || loadedSerial(t, reloaded) != after {
		t.Fatalf("reload after renew: err %v", err)
	}
}

// A crash after the renewed pair was staged but before it was moved into
// place must not strand the node on the superseded certificate.
func TestLoadIdentityPromotesStagedRenewal(t *testing.T) {
	f := newMasterFixture(t)
	dir, nodeID := joinedIdentity(t, f)
	key, crt, serial := newerPair(t, f, nodeID)
	if err := os.WriteFile(filepath.Join(dir, "node.key.new"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.crt.new"), crt, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedSerial(t, id); got != serial {
		t.Fatalf("loaded serial %s, want the staged %s", got, serial)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "node.crt")); string(b) != string(crt) {
		t.Fatal("staged cert not promoted to node.crt")
	}
	assertMode(t, filepath.Join(dir, "node.key"), 0o600)
}

// A crash between the two renames leaves the new key in place and the new
// cert still staged.
func TestLoadIdentityRecoversHalfPromotedRenewal(t *testing.T) {
	f := newMasterFixture(t)
	dir, nodeID := joinedIdentity(t, f)
	key, crt, serial := newerPair(t, f, nodeID)
	if err := os.WriteFile(filepath.Join(dir, "node.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node.crt.new"), crt, 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := loadedSerial(t, id); got != serial {
		t.Fatalf("loaded serial %s, want %s", got, serial)
	}
}

// failingApplySink makes Apply fail while fail is set (the master's disk is
// refusing writes) while live updates keep working.
type failingApplySink struct {
	Sink
	fail *atomic.Bool
}

func (s failingApplySink) Apply(id string, recs []Record) error {
	if s.fail.Load() {
		return errors.New("injected apply failure")
	}
	return s.Sink.Apply(id, recs)
}

// While live updates get through but the data lane is backing off with
// unacked records, the link reads "catching up", never "linked"; once the
// backlog drains it is "linked" again.
func TestShipperCatchingUpWhileDataLaneRetries(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	f := newMasterFixture(t, func(c *MasterConfig) { c.Sink = failingApplySink{Sink: c.Sink, fail: &fail} })
	ob, _ := OpenOutbox(t.TempDir(), 64<<20)
	appendN(t, ob, 1, 3)
	sh, _ := startShipperWithFastBackoff(t, f, ob, nil)
	waitFor(t, "catching up", func() bool { return sh.Status().State == "catching up" })
	for i := 0; i < 30; i++ { // across several live ticks
		if st := sh.Status(); st.State != "catching up" || st.LastError == "" {
			t.Fatalf("status %+v, want catching up with the data error", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fail.Store(false)
	waitFor(t, "linked", func() bool { return sh.Status().State == "linked" })
	if st := sh.Status(); st.Outbox.Unacked != 0 || st.LastError != "" {
		t.Fatalf("status after drain %+v", st)
	}
}
