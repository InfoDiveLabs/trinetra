package serverwatch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
