package trinetra

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

func testDeps(t *testing.T, dir string) (fleetDeps, *[]Alert) {
	var mu sync.Mutex
	var alerts []Alert
	record := func(a Alert) { mu.Lock(); alerts = append(alerts, a); mu.Unlock() }
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{CPU: 12} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)
	return fleetDeps{
		stateDir: dir, getCfg: func() *config.Config { return cfg }, self: self,
		latestSnapshot: func() Snapshot { return Snapshot{CPU: 12} },
		alog:           NewAlertLog(filepath.Join(dir, "alertlog.jsonl")),
		alertStatePath: filepath.Join(dir, "alerts.json"),
		alert:          record,
		deliverSync:    func(a Alert) bool { record(a); return true },
		alertFallback:  func(a Alert, _ *pushedSilences) { record(a) },
		logf:           t.Logf,
	}, &alerts
}

// TestStartMasterWarnsOnceAboutDuplicateRegistryNames is the review round-2
// item (b) regression test: an EXISTING registry.json (from before names
// were unique) is loaded as-is -- no migration, no auto-rename -- but
// startMaster logs one warning line naming the duplicates.
func TestStartMasterWarnsOnceAboutDuplicateRegistryNames(t *testing.T) {
	dir := t.TempDir()
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(fleetMasterDir(dir), "registry.json")
	if err := os.MkdirAll(filepath.Dir(regPath), 0o700); err != nil {
		t.Fatal(err)
	}
	dupNodes := []fleet.Node{
		{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "web1"},
		{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "Web1"},
	}
	b, err := json.Marshal(dupNodes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regPath, b, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var lines []string
	cfg := config.Default()
	self := newInprocAPI(func() Snapshot { return Snapshot{} }, func() *config.Config { return cfg }, nil, dir, nil, nil, nil)
	d := fleetDeps{
		stateDir: dir, getCfg: func() *config.Config { return cfg }, self: self,
		latestSnapshot: func() Snapshot { return Snapshot{} },
		alog:           NewAlertLog(filepath.Join(dir, "alertlog.jsonl")),
		alertStatePath: filepath.Join(dir, "alerts.json"),
		alert:          func(Alert) {}, deliverSync: func(Alert) bool { return true },
		alertFallback: func(Alert, *pushedSilences) {},
		logf: func(format string, args ...any) {
			mu.Lock()
			lines = append(lines, fmt.Sprintf(format, args...))
			mu.Unlock()
		},
	}
	cfg.Fleet.Role = config.RoleMaster
	cfg.Fleet.Address = "127.0.0.1"
	cfg.Fleet.Listen = "127.0.0.1:0"
	rt := startFleet(context.Background(), cfg, d)
	t.Cleanup(rt.stop)
	if rt.provider.master == nil {
		t.Fatal("master did not start")
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		low := strings.ToLower(l)
		if strings.Contains(l, "WARNING") && strings.Contains(low, "web1") && strings.Contains(low, "aaaaaaaa") && strings.Contains(low, "bbbbbbbb") {
			found = true
		}
	}
	if !found {
		t.Fatalf("log lines = %v, want one WARNING line naming both duplicate-name nodes", lines)
	}
	// No migration: both nodes keep their original (still-duplicate) names.
	n1, ok1 := rt.provider.master.reg.Get(dupNodes[0].ID)
	n2, ok2 := rt.provider.master.reg.Get(dupNodes[1].ID)
	if !ok1 || !ok2 || n1.Name != "web1" || n2.Name != "Web1" {
		t.Fatalf("registry nodes = %+v(%v) %+v(%v), want unchanged", n1, ok1, n2, ok2)
	}
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
	if !reflect.DeepEqual(a, want) {
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

	// task 6 part 3: SetNodeDeps validates self-dependency and unknown
	// node ids, accepts a mix of node ids and "tag:<t>" entries, and audits.
	if err := fa.SetNodeDeps(live, []string{live}, "op"); err == nil {
		t.Fatal("self-dependency accepted")
	}
	if err := fa.SetNodeDeps(live, []string{"no-such-node"}, "op"); err == nil {
		t.Fatal("unknown dependency accepted")
	}
	if err := fa.SetNodeDeps(live, []string{quiet, "tag:eu-1"}, "op"); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.reg.Get(live); len(n.DependsOn) != 2 || n.DependsOn[0] != quiet || n.DependsOn[1] != "tag:eu-1" {
		t.Fatalf("deps = %v", n.DependsOn)
	}
	auditEntries, err := fa.Audit(0)
	if err != nil {
		t.Fatal(err)
	}
	foundDepsAudit := false
	for _, e := range auditEntries {
		if e.Action == "fleet.node.deps" && e.Target == live && e.Actor == "op" {
			foundDepsAudit = true
		}
	}
	if !foundDepsAudit {
		t.Fatalf("no fleet.node.deps audit entry in %+v", auditEntries)
	}
	if err := fa.SetNodeDeps(live, nil, "op"); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.reg.Get(live); len(n.DependsOn) != 0 {
		t.Fatalf("deps after clear = %v", n.DependsOn)
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
			"SetNodeDeps": fa.SetNodeDeps("x", nil, "op"),
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
	loop := newMasterLoop(reg, tracker, newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default()), nil), nil, d, now)

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

// TestMasterLoopNodeDownAlertGoesThroughEngine covers task 3's "masterLoop's
// own fleet alerts go through the same engine as a child's": with a real
// fleetAlertEngine wired as loop.alert, a node-down fire must both reach
// d.alert (via the engine's deliver) AND create a firing incident, keyed
// "self:<alert key>" since a master-generated alert has no source node.
func TestMasterLoopNodeDownAlertGoesThroughEngine(t *testing.T) {
	disableGroupWaitForTest(t)
	dir := t.TempDir()
	d, alerts := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	tracker.Seed(nil, nil, time.Now().Unix())
	now := time.Now()
	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default()), nil)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	engine := newFleetAlertEngine(func() time.Time { return now }, d.deliverSync,
		func(string, fleet.Frame) bool { return false }, func(string) bool { return false }, incidents)
	loop := newMasterLoop(reg, tracker, sink, engine, d, now)

	oldID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: oldID, Name: "ghost", Joined: now.Unix() - 3600}); err != nil {
		t.Fatal(err)
	}
	loop.tick(now)
	// Submit's actual delivery (steps 2-4) runs off its own goroutine now
	// (B3 review round 1); wait for it before asserting on *alerts.
	engine.waitIdleForTest()

	if len(*alerts) != 1 {
		t.Fatalf("alerts = %+v, want one node-down (delivered via the engine)", *alerts)
	}
	incs := incidents.List(core.IncidentFilter{}, nil)
	// (task 6 part 2) The default group key is (rule, severity) -- a
	// master-own node-down alert's key already embeds the target node id,
	// so this bucket is still unique to this one node (see ruleFromKey's doc
	// comment), just no longer formatted as the old "self:<key>" per-alert
	// identity.
	wantGroupKey := "rule=fleet:node:" + oldID + ":down|severity=critical"
	if len(incs) != 1 || incs[0].GroupKey != wantGroupKey || incs[0].State != "firing" {
		t.Fatalf("incidents = %+v, want one firing incident with group key %q", incs, wantGroupKey)
	}
}

// TestMasterLoopTickPushesLeasesExcludingRevoked covers the lease cadence
// ruling: tick pushes a lease to every connected, non-revoked node; a
// revoked node gets none.
func TestMasterLoopTickPushesLeasesExcludingRevoked(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	now := time.Now()
	tracker.Seed(nil, nil, now.Unix())
	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default()), nil)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	pushed := map[string]int{}
	engine := newFleetAlertEngine(func() time.Time { return now }, d.deliverSync,
		func(node string, f fleet.Frame) bool {
			if f.Type != "lease" {
				return true
			}
			mu.Lock()
			pushed[node]++
			mu.Unlock()
			return true
		},
		func(string) bool { return true }, incidents)
	loop := newMasterLoop(reg, tracker, sink, engine, d, now)

	goodID, _ := fleet.NewNodeID()
	badID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: goodID, Name: "good", Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(fleet.Node{ID: badID, Name: "bad", Revoked: true, Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	tracker.Seen(goodID, now.Unix(), 0)
	tracker.Seen(badID, now.Unix(), 0)
	tracker.SetRevoked(badID, true)

	loop.tick(now)

	mu.Lock()
	defer mu.Unlock()
	if pushed[goodID] != 1 {
		t.Fatalf("good node leases = %d, want 1", pushed[goodID])
	}
	if pushed[badID] != 0 {
		t.Fatalf("revoked node leases = %d, want 0", pushed[badID])
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

// `fleet node remove` drops a node from the registry and liveness tracking,
// resolves its open node-down page, and keeps its replicated history.
func TestFleetRemoveNodeResolvesDownAlertAndKeepsReplica(t *testing.T) {
	dir := t.TempDir()
	d, alerts := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	now := time.Now()
	loop := newMasterLoop(reg, tracker, sink, nil, d, now)
	p := &fleetProvider{self: d.self, role: config.RoleMaster, selfName: func() string { return "m" },
		master: &masterState{reg: reg, sink: sink, tracker: tracker, loop: loop, getCfg: d.getCfg}}
	id, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: id, Name: "gone-box", Joined: now.Unix() - 3600}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(id, []fleet.Record{samplesRec(1, 100, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	loop.tick(now)
	if len(*alerts) != 1 || (*alerts)[0].Kind != "fire" {
		t.Fatalf("setup alerts = %+v", *alerts)
	}
	if err := p.Fleet().RemoveNode(id); err != nil {
		t.Fatal(err)
	}
	if len(*alerts) != 2 || (*alerts)[1].Kind != "recover" || (*alerts)[1].Key != "fleet:node:"+id+":down" {
		t.Fatalf("alerts after remove = %+v", *alerts)
	}
	if _, ok := reg.Get(id); ok {
		t.Fatal("node still in registry")
	}
	if s := tracker.State(id); s != "" {
		t.Fatalf("tracker state = %q", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "nodes", id)); err != nil {
		t.Fatalf("replica data not kept: %v", err)
	}
	loop.tick(now.Add(10 * time.Second))
	if len(*alerts) != 2 {
		t.Fatalf("removed node paged again: %+v", *alerts)
	}
	if err := p.Fleet().RemoveNode(id); err != core.ErrNoSuchNode {
		t.Fatalf("second remove err = %v", err)
	}
}

// A child whose clock is far off is flagged lagging, surfaced in the node
// list with its drop counters, and warned about once (not every request).
func TestMasterSkewWarnsOnceMarksLaggingAndSurfaces(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	var logs []string
	d.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	reg, _ := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	sink := newReplicaSink(filepath.Join(dir, "nodes"), StoreOptions{}, nil)
	now := time.Now()
	loop := newMasterLoop(reg, tracker, sink, nil, d, now)
	id, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: id, Name: "fast-clock", Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	tracker.Seen(id, now.Unix(), 0)
	for i := 0; i < 3; i++ {
		loop.observeSkew(id, now, now.Unix()+90) // child clock 90s ahead
	}
	warned := 0
	for _, l := range logs {
		if strings.Contains(l, "clock") && strings.Contains(l, "fast-clock") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("skew warnings = %d, want 1: %q", warned, logs)
	}
	loop.tick(now)
	if s := tracker.State(id); s != fleet.StateLagging {
		t.Fatalf("state = %q, want lagging", s)
	}
	if err := sink.Apply(id, []fleet.Record{samplesRec(1, 100, map[string]float64{"cpu": 1}), samplesRec(2, 90, map[string]float64{"cpu": 2})}); err != nil {
		t.Fatal(err)
	}
	p := &fleetProvider{self: d.self, role: config.RoleMaster, selfName: func() string { return "m" },
		master: &masterState{reg: reg, sink: sink, tracker: tracker, loop: loop, getCfg: d.getCfg}}
	ns, _ := p.Fleet().Nodes(core.NodeFilter{})
	var n core.NodeSummary
	for _, s := range ns {
		if s.ID == id {
			n = s
		}
	}
	if n.SkewSec != -90 || n.DroppedOutOfOrder != 1 || n.State != string(fleet.StateLagging) {
		t.Fatalf("node summary = %+v, want skew -90, 1 dropped, lagging", n)
	}
}

// The master warns at start when its server certificate is within 90 days
// of expiry, and says how to re-issue it without re-enrolling children.
func TestServerLeafExpiryWarning(t *testing.T) {
	dir := t.TempDir()
	issued := time.Now()
	if err := fleetInitPKI(dir, []string{"127.0.0.1"}, "test", issued); err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(filepath.Join(fleetPKIDir(dir), "server.crt"), filepath.Join(fleetPKIDir(dir), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	if w := serverLeafExpiryWarning(leaf, issued); w != "" {
		t.Fatalf("fresh leaf warned: %s", w)
	}
	nearEnd := issued.Add(fleet.ServerCertLife - 30*24*time.Hour)
	w := serverLeafExpiryWarning(leaf, nearEnd)
	if !strings.Contains(w, "expires") || !strings.Contains(w, "fleet disable") || !strings.Contains(w, "fleet init") {
		t.Fatalf("warning = %q", w)
	}
}

// dropWarnFixture is a master loop over a replica sink in dir with one
// registered node, logging into logs.
type dropWarnFixture struct {
	dir     string
	d       fleetDeps
	reg     *fleet.Registry
	tracker *fleet.Tracker
	id      string
	logs    *[]string
}

func newDropWarnFixture(t *testing.T) *dropWarnFixture {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	logs := &[]string{}
	d.logf = func(format string, args ...any) { *logs = append(*logs, fmt.Sprintf(format, args...)) }
	reg, _ := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	id, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: id, Name: "web-9", Joined: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	return &dropWarnFixture{dir: dir, d: d, reg: reg, tracker: fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute)), id: id, logs: logs}
}

// start builds a master loop over a fresh sink on the same directory, as a
// master (re)start does.
func (f *dropWarnFixture) start(now time.Time) (*masterLoop, *replicaSink) {
	sink := newReplicaSink(filepath.Join(f.dir, "nodes"), StoreOptions{}, nil)
	return newMasterLoop(f.reg, f.tracker, sink, nil, f.d, now), sink
}

func (f *dropWarnFixture) warnings() int {
	n := 0
	for _, l := range *f.logs {
		if strings.Contains(l, "WARNING") && strings.Contains(l, "web-9") && strings.Contains(l, "out of order") {
			n++
		}
	}
	return n
}

// The master logs a warning when a node's replica drops points out of
// order (or over the series limit) during a maintenance interval, once per
// increase; duplicates from refills are not warned about.
func TestMasterWarnsWhenOutOfOrderDropsGrow(t *testing.T) {
	f := newDropWarnFixture(t)
	now := time.Now()
	loop, sink := f.start(now)
	if err := sink.Apply(f.id, []fleet.Record{samplesRec(1, 100, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	// A duplicate only.
	if err := sink.Backfill(f.id, []fleet.Record{samplesRec(0, 100, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now)
	if w := f.warnings(); w != 0 {
		t.Fatalf("warned %d times for a duplicate: %q", w, *f.logs)
	}
	if err := sink.Backfill(f.id, []fleet.Record{samplesRec(0, 50, map[string]float64{"cpu": 1}), samplesRec(0, 60, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	loop.tick(now.Add(masterTickInterval)) // mid-interval: not yet
	if w := f.warnings(); w != 0 {
		t.Fatalf("warned before the interval ended: %q", *f.logs)
	}
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now)
	logs := *f.logs
	if w := f.warnings(); w != 1 || !strings.Contains(logs[len(logs)-1], "2 points") {
		t.Fatalf("warnings = %d, want 1 naming 2 points: %q", w, logs)
	}
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now)
	if w := f.warnings(); w != 1 {
		t.Fatalf("warned again without new drops: %q", *f.logs)
	}
}

// The warning baseline is persisted: drops that happen after the last
// check but before a master restart are still warned about after it, and
// drops already warned about are not warned about again.
func TestMasterDropWarningSurvivesRestart(t *testing.T) {
	f := newDropWarnFixture(t)
	now := time.Now()
	loop, sink := f.start(now)
	if err := sink.Apply(f.id, []fleet.Record{samplesRec(1, 100, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now) // baseline: nothing dropped yet
	if err := sink.Backfill(f.id, []fleet.Record{samplesRec(0, 50, map[string]float64{"cpu": 1})}); err != nil {
		t.Fatal(err)
	}
	// Restart before the next check.
	loop, _ = f.start(now)
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now)
	if w := f.warnings(); w != 1 {
		t.Fatalf("warnings after restart = %d, want 1: %q", w, *f.logs)
	}
	// Another restart: that drop was already warned about.
	loop, _ = f.start(now)
	now = now.Add(storeMaintenanceInterval)
	loop.tick(now)
	if w := f.warnings(); w != 1 {
		t.Fatalf("already-warned drop warned again after restart: %q", *f.logs)
	}
}

// TestFleetDepsAlertGoesThroughAsyncQueueNotSyncDispatch is the B3 review
// round 2 minor: fleetDeps.alert (used by a child's own childLinkAlerts,
// fleet_daemon.go's startChild) must stay the ordinary async enqueueAndLog
// path -- it must never block its caller on a slow/blocked channel, unlike
// fleetDeps.deliverSync (the master engine's synchronous path, which is
// SUPPOSED to block until dispatch completes).
func TestFleetDepsAlertGoesThroughAsyncQueueNotSyncDispatch(t *testing.T) {
	dir := t.TempDir()
	alog := NewAlertLog(filepath.Join(dir, "alertlog.jsonl"))
	bus := newEventBus()
	notifier := &fakeNotifier{name: "slow", block: 200 * time.Millisecond}
	disp := NewDispatcher([]Channel{allowAllChannel(notifier)}, time.Second)
	q := NewNotifierQueue(disp, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	alert := func(a Alert) { enqueueAndLog(alog, bus, q, a, false) }
	deliverSync := func(a Alert) bool { return deliverSyncAndLog(alog, bus, q, a, false) }

	// alert (the child link-alert path) must return almost immediately,
	// well before the slow notifier's 200ms -- it only enqueues.
	start := time.Now()
	alert(Alert{Key: "fleet:link:down", Title: "link down", Kind: "fire", Time: time.Now().Unix()})
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("alert() took %s -- it must enqueue asynchronously, not block on dispatch", elapsed)
	}
	// ... but the alert IS eventually actually dispatched by the queue's own
	// worker goroutine.
	deadline := time.Now().Add(2 * time.Second)
	for len(notifier.received()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("alert() was never actually dispatched by the async queue")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// deliverSync (the master engine's path), by contrast, blocks until the
	// same slow notifier actually completes.
	start = time.Now()
	ok := deliverSync(Alert{Key: "fleet:node:x:down", Title: "x is down", Kind: "fire", Time: time.Now().Unix()})
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("deliverSync() returned after %s -- it must block for the dispatch to complete", elapsed)
	}
	if !ok {
		t.Fatal("deliverSync() = false, want true (the notifier succeeds)")
	}
}

// TestMasterLoopStillActiveVariants covers stillActive's per-key rules
// directly (B3 review round 2 1(b)): a down node's key is active, an
// online/revoked/removed node's key is not, fleet:connectivity follows
// MassDown, and an unrecognized key defaults to "still active" (left alone,
// for a future rule alert per B7 to plug in later).
func TestMasterLoopStillActiveVariants(t *testing.T) {
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(2 * time.Minute))
	now := time.Now()
	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default()), nil)
	loop := newMasterLoop(reg, tracker, sink, nil, d, now)

	onlineID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: onlineID, Name: "n", Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	tracker.Seen(onlineID, now.Unix(), 0)

	revokedID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: revokedID, Name: "r", Revoked: true, Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}

	downID, _ := fleet.NewNodeID()
	if err := reg.Add(fleet.Node{ID: downID, Name: "d", Joined: now.Unix()}); err != nil {
		t.Fatal(err)
	}
	tracker.Seen(downID, now.Unix()-1000, 0) // long enough ago to classify as down
	tracker.Evaluate(now.Unix())             // transitions are only applied on Evaluate

	goneID, _ := fleet.NewNodeID() // never added: "removed"

	cases := []struct {
		key  string
		want bool
	}{
		{"fleet:node:" + onlineID + ":down", false},
		{"fleet:node:" + revokedID + ":down", false},
		{"fleet:node:" + goneID + ":down", false},
		{"fleet:node:" + downID + ":down", true},
		{"fleet:connectivity", false}, // no MassDown in the zero-value Evaluation below
	}
	for _, c := range cases {
		if got := loop.stillActive(c.key, fleet.Evaluation{}); got != c.want {
			t.Errorf("stillActive(%q) = %v, want %v", c.key, got, c.want)
		}
	}
	if !loop.stillActive("fleet:connectivity", fleet.Evaluation{MassDown: []string{onlineID}}) {
		t.Error("stillActive(fleet:connectivity) with MassDown set should be true")
	}
	if !loop.stillActive("some:future:rule:key", fleet.Evaluation{}) {
		t.Error("an unrecognized key should default to 'still active' (left alone)")
	}
}

// TestMasterLoopRecoversOrphanedDownIncidentAfterBlindWindow covers B3
// review round 2 1(b): an incident left "firing" for a node that is
// actually online again (e.g. recorded before a restart wiped the
// in-memory NodeAlerter/tracker state that would have noticed and emitted
// the matching recover itself) must be reconciled -- but only once the
// blind window (node_down_after since this masterLoop started) has passed,
// giving the tracker a real chance to observe the node's true state first.
func TestMasterLoopRecoversOrphanedDownIncidentAfterBlindWindow(t *testing.T) {
	disableGroupWaitForTest(t)
	dir := t.TempDir()
	d, _ := testDeps(t, dir)
	reg, err := fleet.OpenRegistry(filepath.Join(dir, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	nodeDownAfter := config.Default().FleetNodeDownAfter()
	tracker := fleet.NewTracker(fleet.DefaultTrackerConfig(nodeDownAfter))
	started := time.Now()
	tracker.Seed(nil, nil, started.Unix())
	sink := newReplicaSink(filepath.Join(dir, "nodes"), storeOptionsFor(config.Default()), nil)
	incidents, err := loadIncidentStore(filepath.Join(dir, "incidents.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	engine := newFleetAlertEngine(func() time.Time { return started }, d.deliverSync,
		func(string, fleet.Frame) bool { return false }, func(string) bool { return false }, incidents)
	loop := newMasterLoop(reg, tracker, sink, engine, d, started)

	nodeID, err := fleet.NewNodeID()
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Add(fleet.Node{ID: nodeID, Name: "web1", Joined: started.Unix()}); err != nil {
		t.Fatal(err)
	}
	// An orphaned incident: recorded as firing, as if from before a
	// restart, while the node itself is (and, per tracker.Seen below,
	// always was in this test) online.
	key := "fleet:node:" + nodeID + ":down"
	// groupKey matches EXACTLY what a real fire through the engine would
	// have computed (groupKeyFor's default, task 6 part 2) -- this direct
	// Apply call is simulating "recorded before a restart", and the
	// reconciling recover below goes through the real engine/Submit, which
	// must find this exact incident open under that same bucket.
	if _, err := incidents.Apply(incidentApply{
		src: alertSource{}, alert: Alert{Key: key, Title: "🔴 web1 is down", Severity: SevCritical, Kind: "fire", Time: started.Unix()},
		firedAt: started.Unix(), now: started.Unix(),
		groupKey: groupKeyFor(nil, alertSource{}, Alert{Key: key, Severity: SevCritical}),
	}); err != nil {
		t.Fatal(err)
	}
	tracker.Seen(nodeID, started.Unix(), 0)

	// Before the blind window elapses, tick must not touch it.
	loop.tick(started)
	engine.waitIdleForTest()
	if incs := incidents.List(core.IncidentFilter{State: "firing"}, nil); len(incs) != 1 {
		t.Fatalf("orphan check ran before the blind window elapsed: %+v", incs)
	}

	// After the blind window, the next tick reconciles it.
	after := started.Add(nodeDownAfter + time.Second)
	tracker.Seen(nodeID, after.Unix(), 0)
	loop.tick(after)
	engine.waitIdleForTest()

	if incs := incidents.List(core.IncidentFilter{State: "firing"}, nil); len(incs) != 0 {
		t.Fatalf("orphaned incident not recovered after the blind window: %+v", incs)
	}
	resolved := incidents.List(core.IncidentFilter{State: "resolved"}, nil)
	wantGroupKey := groupKeyFor(nil, alertSource{}, Alert{Key: key, Severity: SevCritical})
	if len(resolved) != 1 || resolved[0].GroupKey != wantGroupKey {
		t.Fatalf("resolved incidents = %+v", resolved)
	}

	// A second tick must not re-run the (one-shot) orphan check.
	loop.tick(after.Add(masterTickInterval))
	engine.waitIdleForTest()
	if got := incidents.List(core.IncidentFilter{}, nil); len(got) != 1 {
		t.Fatalf("orphan check ran a second time: %+v", got)
	}
}
