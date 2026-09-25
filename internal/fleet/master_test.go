package fleet

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	return nil
}
func (s *fakeSink) Backfill(id string, recs []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfill[id] = append(s.backfill[id], recs...)
	return nil
}
func (s *fakeSink) Live(id string, u LiveUpdate) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[id] = u
	return nil
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

func newMasterFixture(t *testing.T) *masterFixture {
	t.Helper()
	ca, leaf := newTestPKI(t)
	dir := t.TempDir()
	reg, _ := OpenRegistry(filepath.Join(dir, "registry.json"))
	toks, _ := OpenTokens(filepath.Join(dir, "tokens.json"))
	f := &masterFixture{ca: ca, reg: reg, toks: toks, sink: newFakeSink(), pin: SPKIPin(ca.Cert)}
	m := NewMaster(MasterConfig{
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
	})
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
