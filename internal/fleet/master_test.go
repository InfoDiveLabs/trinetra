package fleet

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
