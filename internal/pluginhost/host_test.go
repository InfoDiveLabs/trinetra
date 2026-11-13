package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type hostRig struct {
	h    *Host
	reg  *Registrar
	tk   *control.Tokens
	inv  *fakeInv
	core *fakeCore
}

func newHostRig(t *testing.T) *hostRig {
	t.Helper()
	tk := control.NewTokens()
	reg := NewRegistrar(tk)
	inv := newFakeInv()
	dir := t.TempDir()
	fc := &fakeCore{}
	var out []AlertOut
	book, err := OpenAlertBook(filepath.Join(dir, "alerts.json"), func(a AlertOut) { out = append(out, a) }, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHost(HostDeps{
		Inventory: func() plugin.Inventory { return inv }, Registrar: reg,
		Series: NewSeriesGate(dir, func(int64, map[string]float64) bool { return true }, nil),
		Alerts: book, Logs: NewLogSink(nil, nil), Core: fc, NodeID: "node-1", CoreVersion: "v9",
	})
	return &hostRig{h: h, reg: reg, tk: tk, inv: inv, core: fc}
}

func (r *hostRig) enable(t *testing.T, id string, perms ...plugin.Permission) {
	t.Helper()
	r.inv.add(enabledPlugin(id, perms...))
	if _, err := r.reg.Register(id, perms, false); err != nil {
		t.Fatal(err)
	}
}

func TestHelloReturnsGrantAndRejectsIncompatible(t *testing.T) {
	r := newHostRig(t)
	r.enable(t, "probe", plugin.PermMetricsRead)
	resp, err := r.h.Hello("probe", plugin.HelloRequest{HostAPI: "1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.PluginID != "probe" || resp.NodeID != "node-1" || resp.TrinetraVersion != "v9" || resp.HostAPI != "1.0" ||
		len(resp.Granted) != 1 || resp.Granted[0] != plugin.PermMetricsRead {
		t.Fatalf("%+v", resp)
	}
	if !r.h.Connected("probe") {
		t.Fatal("not connected")
	}
	if _, err := r.h.Hello("probe", plugin.HelloRequest{HostAPI: "2.0"}); !errors.Is(err, plugin.ErrIncompatible) {
		t.Fatalf("2.0: %v", err)
	}
	if _, err := r.h.Hello("probe", plugin.HelloRequest{HostAPI: "1.9"}); !errors.Is(err, plugin.ErrIncompatible) {
		t.Fatalf("1.9: %v", err)
	}
	if _, err := r.h.Hello("probe", plugin.HelloRequest{HostAPI: "garbage"}); err == nil {
		t.Fatal("garbage accepted")
	}
	r.inv.add(plugin.Installed{ID: "off", State: plugin.StateInstalled})
	_, _ = r.reg.Register("off", nil, false)
	if _, err := r.h.Hello("off", plugin.HelloRequest{HostAPI: "1.0"}); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("disabled: %v", err)
	}
	_, _ = r.reg.Register("devp", nil, true)
	if resp, err := r.h.Hello("devp", plugin.HelloRequest{HostAPI: "1.0"}); err != nil || !resp.Dev {
		t.Fatalf("dev: %+v %v", resp, err)
	}
	if r.h.Connected("off") {
		t.Fatal("rejected hello marked connected")
	}
}

func TestGetConfigAppliesDefaults(t *testing.T) {
	r := newHostRig(t)
	r.enable(t, "probe")
	r.inv.cfg["probe"] = plugin.ConfigResponse{Revision: 3, Values: map[string]json.RawMessage{}}
	c, err := r.h.GetConfig("probe")
	if err != nil || c.Revision != 3 || string(c.Values["n"]) != "7" {
		t.Fatalf("%+v %v", c, err)
	}
}

func TestWatchConfigLatestWins(t *testing.T) {
	r := newHostRig(t)
	r.enable(t, "probe")
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := r.h.WatchConfig(ctx, "probe")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = r.h.Hello("probe", plugin.HelloRequest{HostAPI: "1.0"})
	for i := 1; i <= 5; i++ {
		r.h.Notify("probe", plugin.ConfigResponse{Revision: int64(i)})
	}
	got := <-ch
	if got.Revision != 5 || string(got.Values["n"]) != "7" {
		t.Fatalf("%+v", got)
	}
	select {
	case extra := <-ch:
		t.Fatalf("extra %+v", extra)
	default:
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for r.h.Connected("probe") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if r.h.Connected("probe") {
		t.Fatal("still connected after stream close")
	}
	r.h.Notify("probe", plugin.ConfigResponse{Revision: 9})
}

func TestSnapshotStripsContainersWithoutDockerRead(t *testing.T) {
	r := newHostRig(t)
	r.core.snap = core.DashboardView{CPU: 5, ContainersRunning: 2, ContainersTotal: 3,
		TopCPUContainers: []core.ContainerView{{Name: "a"}}, TopMemContainers: []core.ContainerView{{Name: "a"}}}
	r.enable(t, "plain", plugin.PermMetricsRead)
	v, err := r.h.Snapshot("plain")
	if err != nil || v.CPU != 5 || v.ContainersRunning != 0 || v.ContainersTotal != 0 || v.TopCPUContainers != nil || v.TopMemContainers != nil {
		t.Fatalf("%+v %v", v, err)
	}
	r.enable(t, "docker", plugin.PermMetricsRead, plugin.PermDockerRead)
	v, _ = r.h.Snapshot("docker")
	if v.ContainersRunning != 2 || len(v.TopCPUContainers) != 1 {
		t.Fatalf("%+v", v)
	}
}

func TestSeriesRefusesOtherPluginNamespace(t *testing.T) {
	r := newHostRig(t)
	r.enable(t, "probe", plugin.PermMetricsRead)
	if _, err := r.h.Series("probe", plugin.SeriesRequest{Metric: "plugin.other.x"}); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("%v", err)
	}
	if _, err := r.h.Series("probe", plugin.SeriesRequest{Metric: "plugin.probe.x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.Series("probe", plugin.SeriesRequest{Metric: "plugin.probexx.x"}); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("prefix confusion: %v", err)
	}
	if _, err := r.h.Series("probe", plugin.SeriesRequest{Metric: "cpu"}); err != nil {
		t.Fatal(err)
	}
}

func TestHostRechecksGrant(t *testing.T) {
	r := newHostRig(t)
	r.enable(t, "bare")
	calls := map[string]func() error{
		"publish": func() error { _, e := r.h.PublishSeries("bare", plugin.PublishRequest{}); return e },
		"raise": func() error {
			return r.h.RaiseAlert("bare", plugin.AlertRequest{Key: "k", Severity: "info", Title: "t"})
		},
		"resolve":  func() error { return r.h.ResolveAlert("bare", plugin.ResolveRequest{Key: "k"}) },
		"snapshot": func() error { _, e := r.h.Snapshot("bare"); return e },
		"series":   func() error { _, e := r.h.Series("bare", plugin.SeriesRequest{Metric: "cpu"}); return e },
		"alerts":   func() error { _, e := r.h.ActiveAlerts("bare"); return e },
		"contain":  func() error { _, e := r.h.Containers("bare"); return e },
		"clogs":    func() error { _, e := r.h.ContainerLogs("bare", plugin.ContainerLogsRequest{Name: "a"}); return e },
		"fleet":    func() error { _, e := r.h.FleetNodes("bare"); return e },
	}
	for name, f := range calls {
		if err := f(); !errors.Is(err, plugin.ErrPermissionDenied) {
			t.Errorf("%s: %v", name, err)
		}
	}
	r.enable(t, "dockeronly", plugin.PermDockerRead)
	if _, err := r.h.ContainerLogs("dockeronly", plugin.ContainerLogsRequest{Name: "a"}); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("needs logs.read too: %v", err)
	}
	r.reg.Unregister("bare")
	if _, err := r.h.GetConfig("bare"); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatalf("revoked: %v", err)
	}
	r.enable(t, "logs", plugin.PermDockerRead, plugin.PermLogsRead)
	if _, err := r.h.ContainerLogs("logs", plugin.ContainerLogsRequest{Name: "a", Lines: 99999}); err != nil || r.core.lastLines != 2000 {
		t.Fatalf("lines %d %v", r.core.lastLines, err)
	}
	if _, err := r.h.FleetNodes("logs"); !errors.Is(err, plugin.ErrPermissionDenied) {
		t.Fatal(err)
	}
	r.enable(t, "fleet", plugin.PermFleetRead)
	if _, err := r.h.FleetNodes("fleet"); !errors.Is(err, core.ErrNotMaster) {
		t.Fatalf("not master: %v", err)
	}
}

func TestContainersMapsMonitoring(t *testing.T) {
	r := newHostRig(t)
	r.core.mon = core.MonitoringView{Containers: []core.MonitoringContainerView{{Name: "web", State: "running", CPUPct: 2, MemMiB: 10}}}
	r.enable(t, "d", plugin.PermDockerRead)
	cs, err := r.h.Containers("d")
	if err != nil || len(cs) != 1 || cs[0].Name != "web" || cs[0].CPUPct != 2 {
		t.Fatalf("%+v %v", cs, err)
	}
}

func TestSilentConnectionClosedAfterHelloTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 10s hello timeout")
	}
	r := newHostRig(t)
	r.enable(t, "probe", plugin.PermMetricsRead)
	sock := filepath.Join(sockDir(t), "h.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = control.ServeAuth(r.core, r.h, ln, r.tk) }()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(conn).ReadByte()
	if err == nil {
		t.Fatal("server sent data to a silent peer")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("connection still open after %v", time.Since(start))
	}
	if r.h.Connected("probe") {
		t.Fatal("silent peer marked connected")
	}
}
