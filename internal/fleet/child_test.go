package fleet

import (
	"context"
	"encoding/json"
	"path/filepath"
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
