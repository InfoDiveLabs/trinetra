package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSink struct {
	mu       sync.Mutex
	applied  map[string]uint64
	recs     map[string][]Record
	backfill map[string][]Record
	live     map[string]LiveUpdate
	// order records each Apply/Backfill call in arrival order, as
	// "apply:<seqs>" / "backfill:<seqs>", so a test can assert call order
	// (e.g. the priority lane's Backfill landing before the backlog's
	// Ingest/Apply) without relying on timing.
	order []string
}

func newFakeSink() *fakeSink {
	return &fakeSink{applied: map[string]uint64{}, recs: map[string][]Record{}, backfill: map[string][]Record{}, live: map[string]LiveUpdate{}}
}
func (s *fakeSink) AppliedSeq(id string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied[id], nil
}
func (s *fakeSink) Apply(id string, recs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[id] = append(s.recs[id], recs...)
	s.applied[id] = recs[len(recs)-1].Seq
	s.order = append(s.order, fmt.Sprintf("apply:%s", seqRangeOf(recs)))
	return nil
}
func (s *fakeSink) Backfill(id string, recs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfill[id] = append(s.backfill[id], recs...)
	s.order = append(s.order, fmt.Sprintf("backfill:%s", seqRangeOf(recs)))
	return nil
}
func (s *fakeSink) Live(id string, u LiveUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[id] = u
	return nil
}

// seqRangeOf renders recs' seqs for order log entries: "a" for one record,
// "a-b" (first-last) for more than one, regardless of whether the run is
// contiguous -- compact enough to print in a test failure even for a
// multi-thousand-record batch.
func seqRangeOf(recs []Record) string {
	if len(recs) == 0 {
		return ""
	}
	if len(recs) == 1 {
		return fmt.Sprintf("%d", recs[0].Seq)
	}
	return fmt.Sprintf("%d-%d", recs[0].Seq, recs[len(recs)-1].Seq)
}

func (s *fakeSink) Order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}
func (s *fakeSink) AppliedFor(id string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied[id]
}
func (s *fakeSink) AppliedRecsFor(id string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.recs[id]...)
}
func (s *fakeSink) BackfillFor(id string) []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.backfill[id]...)
}

type masterFixture struct {
	ca    *CA
	reg   *Registry
	toks  *TokenStore
	sink  *fakeSink
	srv   *httptest.Server
	pin   string
	seen  []string
	seenM sync.Mutex
	skews []int64 // server_time - sent_at per OnSkew call
}

func newMasterFixture(t *testing.T, opts ...func(*MasterConfig)) *masterFixture {
	t.Helper()
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, _ := OpenRegistry(filepath.Join(dir, "registry.json"))
	toks, _ := OpenTokens(filepath.Join(dir, "tokens.json"))
	f := &masterFixture{ca: ca, reg: reg, toks: toks, sink: newFakeSink(), pin: SPKIPin(ca.Cert)}
	mc := MasterConfig{
		CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: f.sink,
		OnContact: func(id string, _ time.Time, _ *LiveUpdate) {
			f.seenM.Lock()
			f.seen = append(f.seen, id)
			f.seenM.Unlock()
		},
		OnSkew: func(id string, now time.Time, sentAt int64) {
			f.seenM.Lock()
			f.skews = append(f.skews, now.Unix()-sentAt)
			f.seenM.Unlock()
		},
	}
	for _, o := range opts {
		o(&mc)
	}
	m := NewMaster(mc)
	f.srv = newTLSServer(t, ca, leaf, m.Handler())
	return f
}

// join performs a full join and returns the node id and a client with its cert.
func (f *masterFixture) join(t *testing.T, tags []string) (string, *http.Client) {
	t.Helper()
	plain, _, err := f.toks.Create(time.Hour, 1, tags, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, csrPEM, _ := NewKeyAndCSR("child")
	body, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csrPEM), Name: "web-1", Version: "v0.5.0"})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("join status %d: %s", resp.StatusCode, b)
	}
	var jr JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr)
	cert, err := tls.X509KeyPair([]byte(jr.Cert), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return jr.NodeID, clientFor(t, f.pin, &cert)
}

func postBatch(t *testing.T, c *http.Client, url string, recs []Record) *http.Response {
	t.Helper()
	b, _ := EncodeBatch(recs)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(HeaderSentAt, fmt.Sprint(time.Now().Unix()))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func seqRecs(from, to uint64) []Record {
	var out []Record
	for s := from; s <= to; s++ {
		out = append(out, Record{Seq: s, Kind: KindAlert, TS: int64(s), Data: json.RawMessage(`{}`)})
	}
	return out
}

func TestJoinRegistersNodeWithTokenTags(t *testing.T) {
	f := newMasterFixture(t)
	id, _ := f.join(t, []string{"prod"})
	n, ok := f.reg.Get(id)
	if !ok || n.Name != "web-1" || len(n.Tags) != 1 || n.Tags[0] != "prod" || n.PubKey == "" || n.Version != "v0.5.0" {
		t.Fatalf("registry node = %+v ok=%v", n, ok)
	}
}

// joinNamed performs a full join with an explicit name and returns the node
// id and the FINAL name the master's JoinResponse reports (review round 2,
// item b: it may be suffixed if it collided).
func joinNamed(t *testing.T, f *masterFixture, name string) (id, finalName string) {
	t.Helper()
	plain, _, err := f.toks.Create(time.Hour, 1, nil, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, csrPEM, _ := NewKeyAndCSR("child")
	body, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csrPEM), Name: name, Version: "v1"})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("join status %d: %s", resp.StatusCode, b)
	}
	var jr JoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		t.Fatal(err)
	}
	return jr.NodeID, jr.Name
}

// TestJoinDedupesNameCaseInsensitive is the review round-2 item (b)
// regression test at the master's HTTP surface: a join whose requested name
// collides (case-insensitively) with an already-registered node's is
// registered under a suffixed name, and the join RESPONSE reports that final
// name (the child prints it, not the one it asked for).
func TestJoinDedupesNameCaseInsensitive(t *testing.T) {
	f := newMasterFixture(t)
	id1, name1 := joinNamed(t, f, "Web1")
	if name1 != "Web1" {
		t.Fatalf("first join name = %q, want unchanged Web1", name1)
	}
	id2, name2 := joinNamed(t, f, "web1")
	if name2 != "web1-2" {
		t.Fatalf("second join name = %q, want web1-2", name2)
	}
	id3, name3 := joinNamed(t, f, "WEB1")
	if name3 != "WEB1-3" {
		t.Fatalf("third join name = %q, want WEB1-3", name3)
	}
	n1, _ := f.reg.Get(id1)
	n2, _ := f.reg.Get(id2)
	n3, _ := f.reg.Get(id3)
	if n1.Name != "Web1" || n2.Name != "web1-2" || n3.Name != "WEB1-3" {
		t.Fatalf("registry names = %q %q %q", n1.Name, n2.Name, n3.Name)
	}
}

func TestJoinRejectsReusedToken(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 1, nil, "t", time.Now())
	for i, want := range []int{200, 403} {
		_, csr, _ := NewKeyAndCSR("c")
		body, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr), Name: "x"})
		resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("attempt %d status %d want %d", i, resp.StatusCode, want)
		}
	}
}

func TestJoinRateLimited(t *testing.T) {
	f := newMasterFixture(t)
	var last int
	for i := 0; i < 6; i++ {
		body, _ := json.Marshal(JoinRequest{Token: "swt_bad", CSR: "x"})
		resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("6th attempt status %d, want 429", last)
	}
}

func TestIngestIsIdempotentAndOrdered(t *testing.T) {
	f := newMasterFixture(t)
	id, c := f.join(t, nil)
	for _, step := range []struct {
		recs []Record
		want uint64
		n    int
	}{
		{seqRecs(1, 3), 3, 3},
		{seqRecs(1, 3), 3, 3}, // replay: nothing new applied
		{seqRecs(2, 5), 5, 5}, // overlap: only 4,5 applied
	} {
		resp := postBatch(t, c, f.srv.URL+PathIngest, step.recs)
		var ir IngestResponse
		json.NewDecoder(resp.Body).Decode(&ir)
		resp.Body.Close()
		if resp.StatusCode != 200 || ir.AckedSeq != step.want {
			t.Fatalf("status %d acked %d want %d", resp.StatusCode, ir.AckedSeq, step.want)
		}
		if got := len(f.sink.recs[id]); got != step.n {
			t.Fatalf("sink has %d records, want %d", got, step.n)
		}
	}
	if len(f.seen) == 0 {
		t.Fatal("OnContact not called")
	}
}

func TestIngestRejectsUnorderedBatch(t *testing.T) {
	f := newMasterFixture(t)
	_, c := f.join(t, nil)
	recs := append(seqRecs(5, 6), seqRecs(3, 3)...)
	resp := postBatch(t, c, f.srv.URL+PathIngest, recs)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
}

func TestIngestRequiresClientCert(t *testing.T) {
	f := newMasterFixture(t)
	resp := postBatch(t, clientFor(t, f.pin, nil), f.srv.URL+PathIngest, seqRecs(1, 1))
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status %d, want 401", resp.StatusCode)
	}
}

func TestIngestRejectsRevokedNode(t *testing.T) {
	f := newMasterFixture(t)
	id, c := f.join(t, nil)
	f.reg.Update(id, func(n *Node) error { n.Revoked = true; return nil })
	resp := postBatch(t, c, f.srv.URL+PathIngest, seqRecs(1, 1))
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestLiveStoresUpdateAndTouches(t *testing.T) {
	f := newMasterFixture(t)
	id, c := f.join(t, nil)
	b, _ := json.Marshal(LiveUpdate{SentAt: time.Now().Unix(), Version: "v9", Snapshot: json.RawMessage(`{"cpu":1}`)})
	resp, err := c.Post(f.srv.URL+PathLive, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if string(f.sink.live[id].Snapshot) != `{"cpu":1}` {
		t.Fatalf("live = %+v", f.sink.live[id])
	}
	if n, _ := f.reg.Get(id); n.Version != "v9" || n.LastSeen == 0 {
		t.Fatalf("registry not touched: %+v", n)
	}
}

func TestBackfillGoesToSink(t *testing.T) {
	f := newMasterFixture(t)
	id, c := f.join(t, nil)
	resp := postBatch(t, c, f.srv.URL+PathBackfill, []Record{{Kind: KindAlert, TS: 1, Data: json.RawMessage(`{}`)}})
	resp.Body.Close()
	if resp.StatusCode != 204 || len(f.sink.backfill[id]) != 1 {
		t.Fatalf("status %d backfill %d", resp.StatusCode, len(f.sink.backfill[id]))
	}
}

func TestRejoinWithPrevKeyKeepsNodeID(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 2, nil, "t", time.Now())
	oldKey, csr1, _ := NewKeyAndCSR("c")
	b1, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr1), Name: "db"})
	resp, _ := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b1))
	var jr1 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr1)
	resp.Body.Close()

	_, csr2, _ := NewKeyAndCSR("c")
	sig, err := SignPrevKey(oldKey, csr2)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr2), Name: "db", PrevNodeID: jr1.NodeID, PrevSig: sig})
	resp, _ = clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b2))
	var jr2 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr2)
	resp.Body.Close()
	if jr2.NodeID != jr1.NodeID {
		t.Fatalf("rejoin got new id %s, want %s", jr2.NodeID, jr1.NodeID)
	}
	if len(f.reg.List()) != 1 {
		t.Fatalf("registry has %d nodes, want 1", len(f.reg.List()))
	}
}

func TestRejoinWithWrongSigGetsNewNodeID(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 2, nil, "t", time.Now())
	_, csr1, _ := NewKeyAndCSR("c")
	b1, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr1), Name: "db"})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b1))
	if err != nil {
		t.Fatal(err)
	}
	var jr1 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr1)
	resp.Body.Close()
	origBefore, _ := f.reg.Get(jr1.NodeID)

	otherKey, _, err := NewKeyAndCSR("other") // unrelated key, not jr1's
	if err != nil {
		t.Fatal(err)
	}
	_, csr2, _ := NewKeyAndCSR("c")
	sig, err := SignPrevKey(otherKey, csr2)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr2), Name: "db2", PrevNodeID: jr1.NodeID, PrevSig: sig})
	resp, err = clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b2))
	if err != nil {
		t.Fatal(err)
	}
	var jr2 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr2)
	resp.Body.Close()

	if jr2.NodeID == jr1.NodeID {
		t.Fatal("wrong PrevSig must not rebind to the original node id")
	}
	origAfter, _ := f.reg.Get(jr1.NodeID)
	if origAfter.PubKey != origBefore.PubKey || origAfter.CertSerial != origBefore.CertSerial {
		t.Fatalf("original node was mutated: before=%+v after=%+v", origBefore, origAfter)
	}
	if len(f.reg.List()) != 2 {
		t.Fatalf("registry has %d nodes, want 2", len(f.reg.List()))
	}
}

func TestRejoinToRevokedNodeGetsNewNodeID(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 2, nil, "t", time.Now())
	oldKey, csr1, _ := NewKeyAndCSR("c")
	b1, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr1), Name: "db"})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b1))
	if err != nil {
		t.Fatal(err)
	}
	var jr1 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr1)
	resp.Body.Close()

	if err := f.reg.Update(jr1.NodeID, func(n *Node) error { n.Revoked = true; return nil }); err != nil {
		t.Fatal(err)
	}

	_, csr2, _ := NewKeyAndCSR("c")
	sig, err := SignPrevKey(oldKey, csr2)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(JoinRequest{Token: plain, CSR: string(csr2), Name: "db2", PrevNodeID: jr1.NodeID, PrevSig: sig})
	resp, err = clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(b2))
	if err != nil {
		t.Fatal(err)
	}
	var jr2 JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr2)
	resp.Body.Close()

	if jr2.NodeID == jr1.NodeID {
		t.Fatal("rejoin to a revoked node must not rebind")
	}
	orig, ok := f.reg.Get(jr1.NodeID)
	if !ok || !orig.Revoked {
		t.Fatalf("original node should remain revoked: %+v ok=%v", orig, ok)
	}
}

func TestRejoinWithUnknownPrevNodeIDGetsNewNodeID(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, _ := f.toks.Create(time.Hour, 1, nil, "t", time.Now())
	_, csr, _ := NewKeyAndCSR("c")
	body, _ := json.Marshal(JoinRequest{
		Token: plain, CSR: string(csr), Name: "x",
		PrevNodeID: "deadbeefdeadbeefdeadbeefdeadbeef", PrevSig: "bm90LWEtcmVhbC1zaWc=",
	})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("join status %d: %s", resp.StatusCode, b)
	}
	var jr JoinResponse
	json.NewDecoder(resp.Body).Decode(&jr)
	if jr.NodeID == "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Fatal("unknown PrevNodeID must not be honored")
	}
	if len(f.reg.List()) != 1 {
		t.Fatalf("registry has %d nodes, want 1", len(f.reg.List()))
	}
}

func TestJoinWithGarbageCSRDoesNotSpendToken(t *testing.T) {
	f := newMasterFixture(t)
	plain, _, err := f.toks.Create(time.Hour, 1, nil, "t", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(JoinRequest{Token: plain, CSR: "not a csr", Name: "x"})
	resp, err := clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage CSR status %d, want 400", resp.StatusCode)
	}

	// The token must still be usable: a bad CSR must not have consumed it.
	_, csr, _ := NewKeyAndCSR("c")
	body, _ = json.Marshal(JoinRequest{Token: plain, CSR: string(csr), Name: "x"})
	resp, err = clientFor(t, f.pin, nil).Post(f.srv.URL+PathJoin, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("valid join after bad CSR status %d: %s", resp.StatusCode, b)
	}
}

func TestServeAndShutdown(t *testing.T) {
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
	m := NewMaster(MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: newFakeSink()})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- m.Serve(ln) }()

	url := fmt.Sprintf("https://%s%s", ln.Addr().String(), PathJoin)
	body, _ := json.Marshal(JoinRequest{Token: "swt_bad", CSR: "x"})
	resp, err := clientFor(t, SPKIPin(ca.Cert), nil).Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request through Serve failed: %v", err)
	}
	resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Serve returned %v, want nil", err)
	}
}

func TestShutdownBeforeServeIsSafe(t *testing.T) {
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, _ := OpenRegistry(filepath.Join(dir, "registry.json"))
	toks, _ := OpenTokens(filepath.Join(dir, "tokens.json"))
	m := NewMaster(MasterConfig{CA: ca, Leaf: leaf, Registry: reg, Tokens: toks, Sink: newFakeSink()})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown before Serve: %v", err)
	}
}

func TestIPLimiterBoundsMapSize(t *testing.T) {
	l := newIPLimiter(5, time.Minute, 3)
	base := time.Now()

	for _, ip := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		if !l.allow(ip, base) {
			t.Fatalf("first hit from %s should be allowed (map under cap)", ip)
		}
	}
	if l.allow("4.4.4.4", base) {
		t.Fatal("4th distinct active IP should be refused once the map is at cap")
	}

	// Existing tracked IPs keep their normal per-IP quota even while the
	// map is at cap and new IPs are being refused.
	for i := 0; i < 3; i++ {
		if !l.allow("1.1.1.1", base.Add(time.Duration(i+1)*time.Second)) {
			t.Fatalf("tracked IP should still get its quota (attempt %d)", i)
		}
	}

	// Once the old entries fall outside the window, a new IP is admitted.
	later := base.Add(2 * time.Minute)
	if !l.allow("5.5.5.5", later) {
		t.Fatal("new IP should be admitted once stale entries are swept")
	}
}

// The master measures clock skew (server_time - sent_at) from the child's
// X-SW-Sent-At header on ingest and backfill, and from sent_at on live.
func TestMasterReportsSkewOnIngestBackfillAndLive(t *testing.T) {
	f := newMasterFixture(t)
	_, c := f.join(t, nil)
	send := func(path string, body []byte, sentAt int64, ct string) {
		t.Helper()
		req, _ := http.NewRequest("POST", f.srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		if sentAt != 0 {
			req.Header.Set(HeaderSentAt, fmt.Sprint(sentAt))
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s status %d", path, resp.StatusCode)
		}
	}
	now := time.Now().Unix()
	batch, _ := EncodeBatch(seqRecs(1, 1))
	send(PathIngest, batch, now-100, "application/x-ndjson")
	send(PathBackfill, batch, now+100, "application/x-ndjson")
	live, _ := json.Marshal(LiveUpdate{SentAt: now - 40})
	send(PathLive, live, 0, "application/json")
	send(PathIngest, batch, 0, "application/x-ndjson") // no header: no sample
	f.seenM.Lock()
	defer f.seenM.Unlock()
	if len(f.skews) != 3 {
		t.Fatalf("skew samples = %v, want 3", f.skews)
	}
	near := func(got, want int64) bool { return got >= want-5 && got <= want+5 }
	if !near(f.skews[0], 100) || !near(f.skews[1], -100) || !near(f.skews[2], 40) {
		t.Fatalf("skews = %v, want ~[100 -100 40]", f.skews)
	}
}

// renewVia renews over c and returns a client presenting the new cert.
func (f *masterFixture) renewVia(t *testing.T, c *http.Client, cn string) *http.Client {
	t.Helper()
	keyPEM, csrPEM, _ := NewKeyAndCSR(cn)
	body, _ := json.Marshal(RenewRequest{CSR: string(csrPEM)})
	resp, err := c.Post(f.srv.URL+PathRenew, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("renew status %d", resp.StatusCode)
	}
	var rr RenewResponse
	json.NewDecoder(resp.Body).Decode(&rr)
	cert, err := tls.X509KeyPair([]byte(rr.Cert), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return clientFor(t, f.pin, &cert)
}

func liveStatus(t *testing.T, f *masterFixture, c *http.Client) int {
	t.Helper()
	b, _ := json.Marshal(LiveUpdate{SentAt: time.Now().Unix()})
	resp, err := c.Post(f.srv.URL+PathLive, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// After a renewal the old certificate keeps working only until the node
// first uses the new one (so a lost renew response cannot lock it out);
// from then on the superseded certificate is refused.
func TestRequireNodeRejectsSupersededCertAfterRenew(t *testing.T) {
	f := newMasterFixture(t)
	id, oldC := f.join(t, nil)
	newC := f.renewVia(t, oldC, id)
	if st := liveStatus(t, f, oldC); st != 200 {
		t.Fatalf("old cert before the new one is used: status %d, want 200", st)
	}
	if st := liveStatus(t, f, newC); st != 200 {
		t.Fatalf("new cert: status %d", st)
	}
	if st := liveStatus(t, f, oldC); st != http.StatusForbidden {
		t.Fatalf("superseded cert: status %d, want 403", st)
	}
	if st := liveStatus(t, f, newC); st != 200 {
		t.Fatalf("new cert after refusal: status %d", st)
	}
}

// A certificate the registry never recorded for this node (e.g. a cert from
// before a re-bind) is refused.
func TestRequireNodeRejectsUnrecordedCert(t *testing.T) {
	f := newMasterFixture(t)
	id, _ := f.join(t, nil)
	stray := issueClient(t, f.ca, id)
	if st := liveStatus(t, f, clientFor(t, f.pin, &stray)); st != http.StatusForbidden {
		t.Fatalf("stray cert: status %d, want 403", st)
	}
}

// slowSink advances a fake clock inside every write, as a slow disk would.
type slowSink struct {
	Sink
	advance func()
}

func (s slowSink) Apply(id string, recs []Record) error { s.advance(); return s.Sink.Apply(id, recs) }
func (s slowSink) Backfill(id string, recs []Record) error {
	s.advance()
	return s.Sink.Backfill(id, recs)
}
func (s slowSink) Live(id string, u LiveUpdate) error { s.advance(); return s.Sink.Live(id, u) }

// The skew sample is taken at request arrival: time spent applying the
// request on the master is not the child's clock being behind.
func TestMasterSkewSampledAtArrival(t *testing.T) {
	var mu sync.Mutex
	clock := time.Now()
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	f := newMasterFixture(t, func(c *MasterConfig) {
		c.Now = now
		c.Sink = slowSink{Sink: c.Sink, advance: func() { mu.Lock(); clock = clock.Add(50 * time.Second); mu.Unlock() }}
	})
	_, c := f.join(t, nil)
	send := func(path string, body []byte, sentAt int64) {
		t.Helper()
		req, _ := http.NewRequest("POST", f.srv.URL+path, bytes.NewReader(body))
		if sentAt != 0 {
			req.Header.Set(HeaderSentAt, fmt.Sprint(sentAt))
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s status %d", path, resp.StatusCode)
		}
	}
	batch, _ := EncodeBatch(seqRecs(1, 1))
	send(PathIngest, batch, now().Unix())
	send(PathBackfill, batch, now().Unix())
	live, _ := json.Marshal(LiveUpdate{SentAt: now().Unix()})
	send(PathLive, live, 0)
	f.seenM.Lock()
	defer f.seenM.Unlock()
	if len(f.skews) != 3 || f.skews[0] != 0 || f.skews[1] != 0 || f.skews[2] != 0 {
		t.Fatalf("skews = %v, want [0 0 0] (sampled before the slow write)", f.skews)
	}
}

type failingLiveSink struct{ Sink }

func (failingLiveSink) Live(string, LiveUpdate) error { return errors.New("disk full") }

// A live update the sink cannot store is logged, not just answered with 500.
func TestMasterLogsLiveSinkFailure(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	f := newMasterFixture(t, func(c *MasterConfig) {
		c.Sink = failingLiveSink{c.Sink}
		c.Logf = func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, fmt.Sprintf(format, args...))
			mu.Unlock()
		}
	})
	_, c := f.join(t, nil)
	live, _ := json.Marshal(LiveUpdate{SentAt: time.Now().Unix()})
	resp, err := c.Post(f.srv.URL+PathLive, "application/json", bytes.NewReader(live))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status %d", resp.StatusCode)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logs, "\n"), "disk full") {
		t.Fatalf("logs = %q", logs)
	}
}

// orderedLiveSink wraps a Sink and records the order Live calls enter/exit
// it. The very first call to enter blocks (on release) until the test lets
// it through, so a second, concurrent Live call for the same node can be
// observed racing ahead of it (or not).
type orderedLiveSink struct {
	Sink
	mu      sync.Mutex
	order   []string
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (s *orderedLiveSink) Live(id string, u LiveUpdate) error {
	s.mu.Lock()
	s.order = append(s.order, "enter:"+u.Version)
	first := !s.armed
	s.armed = true
	s.mu.Unlock()
	if first {
		close(s.entered)
		<-s.release
	}
	err := s.Sink.Live(id, u)
	s.mu.Lock()
	s.order = append(s.order, "exit:"+u.Version)
	s.mu.Unlock()
	return err
}

// TestHandleLiveSerializesConcurrentUpdatesForSameNode reproduces
// debug-step12-report.md's "second, separate issue": unlike
// handleIngest/handleBackfill, handleLive took no per-node lock around
// Sink.Live, so two concurrent Live posts for the same node could enter the
// sink concurrently instead of being serialized.
func TestHandleLiveSerializesConcurrentUpdatesForSameNode(t *testing.T) {
	sink := &orderedLiveSink{entered: make(chan struct{}), release: make(chan struct{})}
	// Always unblock the first call before this test returns, even on a
	// failed assertion: otherwise a blocked handleLive goroutine leaks past
	// the test and stalls the fixture's httptest.Server.Close.
	defer func() {
		select {
		case <-sink.release:
		default:
			close(sink.release)
		}
	}()
	f := newMasterFixture(t, func(c *MasterConfig) {
		sink.Sink = c.Sink
		c.Sink = sink
	})
	_, cl := f.join(t, nil)

	post := func(version string) chan int {
		ch := make(chan int, 1)
		go func() {
			b, _ := json.Marshal(LiveUpdate{SentAt: time.Now().Unix(), Version: version})
			resp, err := cl.Post(f.srv.URL+PathLive, "application/json", bytes.NewReader(b))
			if err != nil {
				t.Error(err)
				ch <- 0
				return
			}
			resp.Body.Close()
			ch <- resp.StatusCode
		}()
		return ch
	}

	firstDone := post("v-first")
	<-sink.entered // first call is inside Sink.Live, blocked there

	secondDone := post("v-second")

	// Give the second call every chance to race ahead of the first if the
	// per-node lock isn't held around Sink.Live.
	time.Sleep(20 * time.Millisecond)
	sink.mu.Lock()
	soFar := append([]string(nil), sink.order...)
	sink.mu.Unlock()
	if len(soFar) != 1 {
		t.Fatalf("Sink.Live call order after 20ms = %v, want exactly 1 entry (the second call must block behind the first on the per-node lock, not race into Sink.Live concurrently)", soFar)
	}

	close(sink.release)
	if st := <-firstDone; st != 200 {
		t.Fatalf("first status %d", st)
	}
	if st := <-secondDone; st != 200 {
		t.Fatalf("second status %d", st)
	}

	want := []string{"enter:v-first", "exit:v-first", "enter:v-second", "exit:v-second"}
	sink.mu.Lock()
	got := append([]string(nil), sink.order...)
	sink.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sink.Live call order = %v, want %v", got, want)
	}
}
