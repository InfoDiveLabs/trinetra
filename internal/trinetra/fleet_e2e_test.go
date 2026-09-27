package trinetra

import (
	"context"
	"crypto/tls"
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// End-to-end store-and-forward tests: a real fleet.Master over TLS with a
// real replicaSink, and a child made of a real tsfile store, outboxTee,
// outbox and shipper. The invariant under test is "no data lost, none
// duplicated": the master's replica must equal the child's local store.

type e2eMaster struct {
	mdir string
	ca   *fleet.CA
	leaf tls.Certificate
	reg  *fleet.Registry
	toks *fleet.TokenStore
	sink *replicaSink
	srv  *httptest.Server
	addr string
}

func (m *e2eMaster) start(t *testing.T, addr string) {
	t.Helper()
	m.sink = newReplicaSink(filepath.Join(m.mdir, "nodes"), StoreOptions{}, nil) // fresh sink = master restart
	fm := fleet.NewMaster(fleet.MasterConfig{CA: m.ca, Leaf: m.leaf, Registry: m.reg, Tokens: m.toks, Sink: m.sink})
	srv := httptest.NewUnstartedServer(fm.Handler())
	if addr != "" {
		var ln net.Listener
		var err error
		for i := 0; i < 250; i++ { // the old listener may take a moment to release the port
			if ln, err = net.Listen("tcp", addr); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("rebind %s: %v", addr, err)
		}
		srv.Listener.Close()
		srv.Listener = ln
	}
	srv.TLS = fleet.ServerTLS(m.leaf, m.ca.Cert)
	srv.StartTLS()
	m.srv, m.addr = srv, srv.Listener.Addr().String()
}

func newE2EMaster(t *testing.T) *e2eMaster {
	t.Helper()
	m := &e2eMaster{mdir: t.TempDir()}
	if err := fleetInitPKI(m.mdir, []string{"127.0.0.1"}, "e2e", time.Now()); err != nil {
		t.Fatal(err)
	}
	pki := fleetPKIDir(m.mdir)
	var err error
	if m.ca, err = fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if m.leaf, err = tls.LoadX509KeyPair(filepath.Join(pki, "server.crt"), filepath.Join(pki, "server.key")); err != nil {
		t.Fatal(err)
	}
	if m.reg, err = fleet.OpenRegistry(filepath.Join(m.mdir, "registry.json")); err != nil {
		t.Fatal(err)
	}
	if m.toks, err = fleet.OpenTokens(filepath.Join(m.mdir, "tokens.json")); err != nil {
		t.Fatal(err)
	}
	m.start(t, "")
	t.Cleanup(func() { m.srv.Close() })
	return m
}

type e2eChild struct {
	store *tsFileStore
	tee   *outboxTee
	ob    *fleet.Outbox
	id    string
	sh    *fleet.Shipper
}

func newE2EChild(t *testing.T, m *e2eMaster, ob *fleet.Outbox) *e2eChild {
	t.Helper()
	cdir := t.TempDir()
	store, err := newTSFileStore(cdir, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := m.toks.Create(time.Hour, 1, nil, "e2e", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code := fleet.EncodeJoin(fleet.JoinInfo{URL: m.srv.URL, Token: plain, Pin: fleet.SPKIPin(m.ca.Cert)})
	res, err := fleet.Join(context.Background(), code, "child", "v-e2e", nil, filepath.Join(cdir, "fleet-child"))
	if err != nil {
		t.Fatal(err)
	}
	ident, err := fleet.LoadIdentity(filepath.Join(cdir, "fleet-child"))
	if err != nil {
		t.Fatal(err)
	}
	c := &e2eChild{store: store, tee: newOutboxTee(ob, t.Logf), ob: ob, id: res.NodeID}
	c.sh = fleet.NewShipper(fleet.ShipperConfig{
		MasterURL: "https://" + m.addr, Pin: fleet.SPKIPin(m.ca.Cert), Identity: ident, Outbox: ob,
		Gaps:      &localGapFiller{store: store, rawRetention: 48 * time.Hour, now: time.Now},
		Live:      func() (fleet.LiveUpdate, error) { return fleet.LiveUpdate{Version: "v-e2e"}, nil },
		LiveEvery: time.Hour, Logf: t.Logf,
	})
	return c
}

// runShipper runs the shipper until the test ends, and waits for it to stop
// so no goroutine logs through t after the test has returned.
func (c *e2eChild) runShipper(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.sh.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// write mimics storeWriter: local append first, then the tee.
func (c *e2eChild) write(t *testing.T, ts int64, v float64) {
	t.Helper()
	ms := MetricSet{"cpu": v}
	if err := c.store.Append(ts, ms); err != nil {
		t.Fatal(err)
	}
	c.tee.Samples(ts, ms)
}

// assertReplicaEqual polls (never a fixed sleep) until the master's replica
// of c holds want points, then requires it to equal the child's local store
// point for point: nothing lost, nothing duplicated, nothing reordered.
func assertReplicaEqual(t *testing.T, m *e2eMaster, c *e2eChild, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		n, err := m.sink.node(c.id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := n.store.Query("cpu", 0, 1<<62, ResRaw)
		if err != nil {
			t.Fatal(err)
		}
		src, err := c.store.Query("cpu", 0, 1<<62, ResRaw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == want && len(src) == want {
			for i := range got {
				if got[i] != src[i] {
					t.Fatalf("point %d differs: master %+v child %+v", i, got[i], src[i])
				}
			}
			return
		}
		if len(got) > want {
			t.Fatalf("replica has %d points, more than the %d written (duplicates)", len(got), want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica has %d points, child %d, want %d", len(got), len(src), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestFleetE2EReplicaMatchesChild(t *testing.T) {
	m := newE2EMaster(t)
	ob, err := fleet.OpenOutbox(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := newE2EChild(t, m, ob)
	c.runShipper(t)
	base := time.Now().Unix() - 1000
	for i := int64(0); i < 100; i++ {
		c.write(t, base+i*5, float64(i))
	}
	assertReplicaEqual(t, m, c, 100)
}

func TestFleetE2EMasterRestartNoLossNoDup(t *testing.T) {
	m := newE2EMaster(t)
	ob, err := fleet.OpenOutbox(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	c := newE2EChild(t, m, ob)
	c.runShipper(t)
	base := time.Now().Unix() - 2000
	for i := int64(0); i < 50; i++ {
		c.write(t, base+i*5, float64(i))
	}
	assertReplicaEqual(t, m, c, 50)

	addr := m.addr
	m.srv.Close() // master goes down
	for i := int64(50); i < 120; i++ {
		c.write(t, base+i*5, float64(i))
	}
	m.start(t, addr) // master restarts on the same address with a fresh sink
	assertReplicaEqual(t, m, c, 120)
}

func TestFleetE2EOutboxOverflowBackfillsFromLocalStore(t *testing.T) {
	m := newE2EMaster(t)
	addr := m.addr
	ob, err := fleet.OpenOutboxSegmented(t.TempDir(), 4000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	c := newE2EChild(t, m, ob)
	m.srv.Close() // master is down from the start: everything spools and overflows
	base := time.Now().Unix() - 3000
	for i := int64(0); i < 300; i++ {
		c.write(t, base+i*5, float64(i))
	}
	gaps := ob.Gaps()
	if len(gaps) == 0 {
		t.Fatal("setup: outbox did not overflow")
	}
	if gaps[0].MinTS != base {
		t.Fatalf("setup: oldest gap starts at ts %d, want the first write %d", gaps[0].MinTS, base)
	}
	m.start(t, addr)
	c.runShipper(t)
	assertReplicaEqual(t, m, c, 300)

	// The replica is exact, so the gap was rebuilt from the local store and
	// the surviving outbox tail followed it; both must now be settled.
	deadline := time.Now().Add(30 * time.Second)
	for len(ob.Gaps()) != 0 || ob.Stats().Unacked != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("outbox not drained: gaps %v stats %+v", ob.Gaps(), ob.Stats())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
