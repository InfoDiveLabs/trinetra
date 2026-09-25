package serverwatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/fleet"
)

func testDeps(t *testing.T, dir string) (fleetDeps, *[]Alert) {
	var alerts []Alert
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{CPU: 12} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)
	return fleetDeps{
		stateDir: dir, getCfg: func() *config.Config { return cfg }, self: self,
		latestSnapshot: func() Snapshot { return Snapshot{CPU: 12} },
		alog:           NewAlertLog(filepath.Join(dir, "alertlog.jsonl")),
		alertStatePath: filepath.Join(dir, "alerts.json"),
		alert:          func(a Alert) { alerts = append(alerts, a) },
		logf:           t.Logf,
	}, &alerts
}

func TestStartFleetSoloCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	before := runtime.NumGoroutine()
	rt := startFleet(context.Background(), config.Default(), d)
	defer rt.stop()
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("solo started goroutines: %d -> %d", before, after)
	}
	if rt.tee != nil {
		t.Fatal("solo has an outbox tee")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("solo created %d entries in state dir: %v", len(ents), ents)
	}
	st, err := rt.provider.Fleet().Status()
	if err != nil || st.Role != config.RoleSolo {
		t.Fatalf("status %+v err %v", st, err)
	}
	nodes, _ := rt.provider.Fleet().Nodes(core.NodeFilter{})
	if len(nodes) != 1 || !nodes[0].Self || nodes[0].CPU != 12 {
		t.Fatalf("nodes = %+v", nodes)
	}
	if _, err := rt.provider.Fleet().CreateToken(core.TokenSpec{}); err != core.ErrNotMaster {
		t.Fatalf("create token on solo err = %v", err)
	}
	if api, err := rt.provider.Node(core.SelfNodeID); err != nil || api == nil {
		t.Fatalf("self node err %v", err)
	}
	var api core.API = &fleetAwareAPI{API: d.self, fleetProvider: rt.provider}
	if _, ok := api.(core.FleetProvider); !ok {
		t.Fatal("fleetAwareAPI does not implement core.FleetProvider")
	}
}

func TestStartFleetMasterWithoutPKIFallsBackToSolo(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	cfg := config.Default()
	cfg.Fleet.Role = config.RoleMaster
	rt := startFleet(context.Background(), cfg, d)
	defer rt.stop()
	if st, _ := rt.provider.Fleet().Status(); st.Role != config.RoleSolo {
		t.Fatalf("role = %q, want solo fallback", st.Role)
	}
}

func TestStartFleetChildWithoutIdentityFallsBackToSolo(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	cfg := config.Default()
	cfg.Fleet.Role = config.RoleChild
	cfg.Fleet.MasterURL = "https://127.0.0.1:1"
	rt := startFleet(context.Background(), cfg, d)
	defer rt.stop()
	if rt.tee != nil {
		t.Fatal("child fallback has an outbox tee")
	}
	if st, _ := rt.provider.Fleet().Status(); st.Role != config.RoleSolo {
		t.Fatalf("role = %q, want solo fallback", st.Role)
	}
	if _, err := os.Stat(filepath.Join(dir, "outbox")); !os.IsNotExist(err) {
		t.Fatalf("child fallback created an outbox: %v", err)
	}
}

func TestStartFleetMasterServesAndTracksNodes(t *testing.T) {
	dir := t.TempDir()
	d, alerts := testDeps(t, dir)
	cfg := config.Default()
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg.Fleet.Role = config.RoleMaster
	cfg.Fleet.Address = "127.0.0.1"
	cfg.Fleet.Listen = "127.0.0.1:0"
	rt := startFleet(context.Background(), cfg, d)
	defer rt.stop()
	st, _ := rt.provider.Fleet().Status()
	if st.Role != config.RoleMaster || st.CAPin == "" {
		t.Fatalf("status = %+v", st)
	}
	ct, err := rt.provider.Fleet().CreateToken(core.TokenSpec{Tags: []string{"lab"}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := fleet.DecodeJoin(ct.JoinCode)
	if err != nil || info.Pin != st.CAPin {
		t.Fatalf("join code %+v err %v", info, err)
	}
	_ = alerts
}

func readPKI(t *testing.T, dir string) (crt, key, srv []byte) {
	t.Helper()
	pki := fleetPKIDir(dir)
	var err error
	if crt, err = os.ReadFile(filepath.Join(pki, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if key, err = os.ReadFile(filepath.Join(pki, "ca.key")); err != nil {
		t.Fatal(err)
	}
	srv, _ = os.ReadFile(filepath.Join(pki, "server.crt"))
	return
}

func caPin(t *testing.T, dir string) string {
	t.Helper()
	pki := fleetPKIDir(dir)
	ca, err := fleet.LoadCA(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	return fleet.SPKIPin(ca.Cert)
}

func TestFleetInitPKIReusesExistingCA(t *testing.T) {
	dir := t.TempDir()
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	pin1 := caPin(t, dir)
	_, _, srv1 := readPKI(t, dir)
	if err := fleetInitPKI(dir, []string{"127.0.0.1", "10.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if pin2 := caPin(t, dir); pin2 != pin1 {
		t.Fatalf("second init rotated the CA pin: %s -> %s", pin1, pin2)
	}
	if _, _, srv2 := readPKI(t, dir); string(srv2) == string(srv1) {
		t.Fatal("second init did not reissue the server leaf")
	}
}

func TestFleetInitPKIRefusesToReplaceBrokenCA(t *testing.T) {
	t.Run("corrupt key", func(t *testing.T) {
		dir := t.TempDir()
		if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
		keyPath := filepath.Join(fleetPKIDir(dir), "ca.key")
		if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		crt0, key0, _ := readPKI(t, dir)
		err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now())
		if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
			t.Fatalf("err = %v, want refusal", err)
		}
		crt1, key1, _ := readPKI(t, dir)
		if string(crt1) != string(crt0) || string(key1) != string(key0) {
			t.Fatal("refused init still touched the CA files")
		}
	})
	t.Run("missing key", func(t *testing.T) {
		dir := t.TempDir()
		if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
		pki := fleetPKIDir(dir)
		crt0, _ := os.ReadFile(filepath.Join(pki, "ca.crt"))
		if err := os.Remove(filepath.Join(pki, "ca.key")); err != nil {
			t.Fatal(err)
		}
		if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err == nil {
			t.Fatal("init with only ca.crt present succeeded")
		}
		crt1, _ := os.ReadFile(filepath.Join(pki, "ca.crt"))
		if string(crt1) != string(crt0) {
			t.Fatal("ca.crt replaced")
		}
		if _, err := os.Stat(filepath.Join(pki, "ca.key")); !os.IsNotExist(err) {
			t.Fatalf("ca.key recreated: %v", err)
		}
	})
}

func TestFleetAlert(t *testing.T) {
	a := fleetAlert(fleet.AlertIntent{Key: "fleet:node:x:down", Title: "x is down", Critical: true}, 1234)
	want := Alert{Key: "fleet:node:x:down", Title: "x is down", Severity: SevCritical, Kind: "fire", Source: "fleet", Time: 1234}
	if a != want {
		t.Fatalf("fire = %+v, want %+v", a, want)
	}
	r := fleetAlert(fleet.AlertIntent{Key: "fleet:node:x:down", Title: "x is back", Recover: true}, 99)
	if r.Kind != "recover" || r.Key != "fleet:node:x:down" || r.Source != "fleet" || r.Time != 99 || r.Severity != SevCritical {
		t.Fatalf("recover = %+v", r)
	}
}

func startTestMaster(t *testing.T, address string) (*fleetRuntime, string) {
	t.Helper()
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Fleet.Role = config.RoleMaster
	cfg.Fleet.Address = address
	cfg.Fleet.Listen = "127.0.0.1:0"
	rt := startFleet(context.Background(), cfg, d)
	t.Cleanup(rt.stop)
	if rt.provider.master == nil {
		t.Fatal("master did not start")
	}
	return rt, dir
}

func addNode(t *testing.T, m *masterState, name string, joined int64) string {
	t.Helper()
	id, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reg.Add(fleet.Node{ID: id, Name: name, Tags: []string{"lab"}, Joined: joined}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMasterProviderNodesAndManagement(t *testing.T) {
	rt, _ := startTestMaster(t, "127.0.0.1")
	m, fa := rt.provider.master, rt.provider.Fleet()
	now := time.Now().Unix()

	live := addNode(t, m, "web1", now)
	quiet := addNode(t, m, "db1", now)
	snap, _ := json.Marshal(Snapshot{CPU: 55, MemPct: 40, Load1: 1.5, Disks: map[string]float64{"/": 30, "/data": 91}})
	if err := m.sink.Live(live, fleet.LiveUpdate{SentAt: now, Snapshot: snap, Outbox: fleet.OutboxStats{Bytes: 2048, Gaps: 1}}); err != nil {
		t.Fatal(err)
	}
	m.tracker.Seen(live, now, 0)

	nodes, err := fa.Nodes(core.NodeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]core.NodeSummary{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if len(nodes) != 3 || !byID[core.SelfNodeID].Self {
		t.Fatalf("nodes = %+v", nodes)
	}
	w := byID[live]
	if w.State != string(fleet.StateOnline) || w.CPU != 55 || w.MemPct != 40 || w.Load1 != 1.5 || w.WorstDiskPct != 91 || w.OutboxBytes != 2048 || w.OutboxGaps != 1 {
		t.Fatalf("live node = %+v", w)
	}
	if q := byID[quiet]; q.State != "unknown" || q.Name != "db1" {
		t.Fatalf("never-contacted node = %+v", q)
	}
	if got, _ := fa.Nodes(core.NodeFilter{Query: "web"}); len(got) != 1 || got[0].ID != live {
		t.Fatalf("filtered = %+v", got)
	}
	if st, _ := fa.Status(); st.Nodes != 3 || st.Role != config.RoleMaster || st.JoinURL == "" {
		t.Fatalf("status = %+v", st)
	}

	if _, err := rt.provider.Node("0123456789abcdef0123456789abcdef"); err != core.ErrNoSuchNode {
		t.Fatalf("Node(nonexistent) err = %v", err)
	}
	if api, err := rt.provider.Node(live); err != nil || api == nil {
		t.Fatalf("Node(live) err = %v", err)
	}

	if err := fa.RenameNode(live, "   "); err == nil {
		t.Fatal("blank rename accepted")
	}
	if err := fa.RenameNode(live, strings.Repeat("x", 65)); err == nil {
		t.Fatal("65-char rename accepted")
	}
	if err := fa.RenameNode(live, "  web-one "); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.reg.Get(live); n.Name != "web-one" {
		t.Fatalf("renamed = %q", n.Name)
	}
	if err := fa.SetNodeTags(live, []string{"Bad Tag"}); err == nil {
		t.Fatal("invalid tag accepted")
	}
	if err := fa.SetNodeTags(live, []string{"prod", "eu-1"}); err != nil {
		t.Fatal(err)
	}

	if err := fa.RevokeNode(quiet); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.reg.Get(quiet); !n.Revoked {
		t.Fatal("registry not revoked")
	}
	m.tracker.Evaluate(time.Now().Unix())
	if s := m.tracker.State(quiet); s != fleet.StateRevoked {
		t.Fatalf("tracker state = %q, want revoked", s)
	}
}

func TestMasterCreateTokenDefaults(t *testing.T) {
	rt, _ := startTestMaster(t, "127.0.0.1")
	fa := rt.provider.Fleet()
	st, _ := fa.Status()
	before := time.Now().Unix()
	ct, err := fa.CreateToken(core.TokenSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if ct.Token.Uses != 1 {
		t.Fatalf("uses = %d, want 1", ct.Token.Uses)
	}
	if d := ct.Token.Expires - before; d < 3600-5 || d > 3600+5 {
		t.Fatalf("ttl = %ds, want ~3600", d)
	}
	info, err := fleet.DecodeJoin(ct.JoinCode)
	if err != nil || info.URL != st.JoinURL || info.Pin != st.CAPin || !strings.HasPrefix(info.Token, "swt_") {
		t.Fatalf("join info %+v err %v (status %+v)", info, err, st)
	}
	toks, _ := fa.Tokens()
	if len(toks) != 1 || toks[0].ID != ct.Token.ID {
		t.Fatalf("tokens = %+v", toks)
	}
	if err := fa.DeleteToken(ct.Token.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMasterEmptyAddressRefusesTokens(t *testing.T) {
	rt, _ := startTestMaster(t, "")
	_, err := rt.provider.Fleet().CreateToken(core.TokenSpec{})
	if err == nil || !strings.Contains(err.Error(), "fleet.address is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestNonMasterProviderRefusesMasterOps(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	for _, role := range []string{config.RoleSolo, config.RoleChild} {
		p := &fleetProvider{self: d.self, role: role, selfName: func() string { return "h" }}
		fa := p.Fleet()
		errs := map[string]error{
			"RenameNode":  fa.RenameNode("x", "y"),
			"SetNodeTags": fa.SetNodeTags("x", nil),
			"RevokeNode":  fa.RevokeNode("x"),
			"DeleteToken": fa.DeleteToken("x"),
		}
		_, errs["Tokens"] = fa.Tokens()
		_, errs["CreateToken"] = fa.CreateToken(core.TokenSpec{})
		for name, err := range errs {
			if err != core.ErrNotMaster {
				t.Errorf("%s on %s: err = %v, want ErrNotMaster", role, name, err)
			}
		}
		if _, err := p.Node("0123456789abcdef0123456789abcdef"); err != core.ErrNoSuchNode {
			t.Errorf("%s Node(remote) err = %v", role, err)
		}
	}
}

func TestMasterLoopAlertsNeverContactedNode(t *testing.T) {
	dir := t.TempDir()
	d, alerts := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	tracker.Seed(nil, nil, time.Now().Unix()) // master start: no nodes yet
	now := time.Now()
	loop := newMasterLoop(reg, tracker, newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default())), d, now)

	oldID, _ := fleet.NewNodeID()
	freshID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: oldID, Name: "ghost", Joined: now.Unix() - 3600}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(fleet.Node{ID: freshID, Name: "newbie", Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	loop.tick(now)
	if len(*alerts) != 1 {
		t.Fatalf("alerts = %+v, want one node-down", *alerts)
	}
	a := (*alerts)[0]
	if a.Key != "fleet:node:"+oldID+":down" || a.Kind != "fire" || !strings.Contains(a.Title, "ghost") {
		t.Fatalf("alert = %+v", a)
	}
	if s := tracker.State(freshID); s != fleet.StateOnline {
		t.Fatalf("fresh node state = %q", s)
	}
}

func TestFleetStopIsIdempotent(t *testing.T) {
	rt, _ := startTestMaster(t, "127.0.0.1")
	done := make(chan struct{})
	go func() { rt.stop(); rt.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stop hung")
	}
}
