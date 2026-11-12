package trinetra

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/plugin/plugintest"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
)

type e2eSink struct {
	mu     sync.Mutex
	series map[string]float64
	alerts []Alert
}

func (s *e2eSink) submit(ts int64, ms MetricSet) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range ms {
		s.series[k] = v
	}
	return true
}

func (s *e2eSink) deliver(a Alert) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, a)
}

func (s *e2eSink) metric(name string) (float64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.series[name]
	return v, ok
}

func (s *e2eSink) count(key, kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.alerts {
		if a.Key == key && a.Kind == kind {
			n++
		}
	}
	return n
}

func e2eWait(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type e2eDaemon struct {
	root, stateDir string
	sink           *e2eSink
	mgr            plugin.Manager
	api            core.API
	stop           func()
}

func startE2EDaemon(t *testing.T, key ed25519.PublicKey, cfg *config.Config) *e2eDaemon {
	t.Helper()
	fastSupervisor(t)
	prevSys, prevRoot, prevSock, prevReg := hasSystemd, pluginIsRoot, pluginrt.SocketRoot, wireRegistryFn
	prevDeps := currentPluginDeps
	t.Cleanup(func() {
		hasSystemd, pluginIsRoot, pluginrt.SocketRoot, wireRegistryFn = prevSys, prevRoot, prevSock, prevReg
		currentPluginDeps = prevDeps
	})
	hasSystemd = func() bool { return false }
	pluginIsRoot = func() bool { return false }
	root := plugintest.ShortDir(t)
	pluginrt.SocketRoot = filepath.Join(root, "p")
	stateDir := filepath.Join(root, "state")
	if key != nil {
		wireRegistryFn = func(d *pluginDeps) {
			d.registry = registry.New(registry.Options{Dir: filepath.Join(d.stateDir, "plugins", "registry"), Keys: []ed25519.PublicKey{key}, Now: d.now})
		}
	}
	sink := &e2eSink{series: map[string]float64{}}
	api := newFileAPI(stateDir, cfg)
	d := &pluginDeps{
		stateDir: stateDir, libDir: filepath.Join(root, "lib"), hostDir: filepath.Join(root, "h"),
		getCfg: func() *config.Config { return cfg }, self: api,
		submitSeries: sink.submit, deliverAlert: sink.deliver,
		logf: t.Logf, now: time.Now, coreVersion: "0.6.0",
	}
	mgr, stop := startPlugins(d)
	t.Cleanup(stop)
	return &e2eDaemon{root: root, stateDir: stateDir, sink: sink, mgr: mgr, api: api, stop: stop}
}

func (e *e2eDaemon) controlClient(t *testing.T) *control.Client {
	t.Helper()
	sock := filepath.Join(e.root, "c.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go control.Serve(&fleetAwareAPI{API: e.api, plugins: e.mgr}, ln, "e2e-control-token")
	c, err := control.Dial(sock, "e2e-control-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPluginEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a plugin binary")
	}
	pkg := plugintest.Build(t, t.TempDir())
	cfg := config.Default()
	if err := cfg.Set("plugins.allow_unverified", "false"); err != nil {
		t.Fatal(err)
	}
	e := startE2EDaemon(t, pkg.Key, cfg)
	mgr := e.mgr

	res, err := mgr.Install(plugin.InstallRequest{Ref: pkg.Path, Actor: "root", Via: "cli"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.ID != plugintest.ID || res.Trust != plugin.TrustVerified {
		t.Fatalf("install result %+v", res)
	}
	if err := mgr.Enable(plugin.EnableRequest{ID: plugintest.ID, Actor: "root", Via: "cli", Accepted: pkg.Manifest.Permissions}); err != nil {
		t.Fatalf("enable: %v", err)
	}

	e2eWait(t, "plugin.e2e.latency in the store", 30*time.Second, func() bool {
		v, ok := e.sink.metric("plugin.e2e.latency")
		return ok && v == 42
	})
	alertKey := "plugin.e2e.probe.down"
	e2eWait(t, "plugin alert delivered", 10*time.Second, func() bool { return e.sink.count(alertKey, "fire") == 1 })
	st, err := mgr.Get(plugintest.ID)
	if err != nil || st.State != plugin.StateEnabled || !st.Running || !st.Health.Connected || st.Trust != plugin.TrustVerified {
		t.Fatalf("status %+v %v", st, err)
	}
	recs, _ := e.api.ActiveAlerts()
	if len(recs) != 1 || recs[0].Key != alertKey {
		t.Fatalf("active alerts %+v", recs)
	}

	c := e.controlClient(t)
	offers, err := c.Plugins().Slots("", plugin.SlotNavPage)
	if err != nil || len(offers) != 1 || offers[0].PluginID != plugintest.ID || offers[0].SlotID != "main" {
		t.Fatalf("slots %+v %v", offers, err)
	}
	rr, err := c.Plugins().RenderSlot(plugin.UIRequest{PluginID: plugintest.ID, Request: plugin.SlotRequest{
		Slot: plugin.SlotNavPage, SlotID: "main", User: plugin.UserContext{Name: "alice", Role: plugin.RoleViewer},
	}})
	if err != nil {
		t.Fatalf("render over control socket: %v", err)
	}
	if len(rr.Widgets) != 2 || rr.Widgets[1].Text == nil || rr.Widgets[1].Text.Text != plugintest.WidgetText {
		t.Fatalf("widgets %+v", rr.Widgets)
	}

	if err := mgr.SetConfig(plugin.SetConfigRequest{ID: plugintest.ID, Values: map[string]string{"mode": "crash"}, Actor: "root", Via: "cli"}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	crashKey := "plugin.e2e.crashloop"
	e2eWait(t, "breaker trip", 30*time.Second, func() bool {
		st, err := mgr.Get(plugintest.ID)
		return err == nil && st.State == plugin.StateFailed
	})
	e2eWait(t, "crash-loop info alert", 5*time.Second, func() bool { return e.sink.count(crashKey, "fire") >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := e.sink.count(crashKey, "fire"); n != 1 {
		t.Fatalf("crash-loop alerts = %d, want 1", n)
	}
	e.sink.mu.Lock()
	for _, a := range e.sink.alerts {
		if a.Key == crashKey && a.Severity != SevInfo {
			t.Errorf("crash-loop alert severity %v", a.Severity)
		}
	}
	e.sink.mu.Unlock()
	if recs, _ := e.api.ActiveAlerts(); len(recs) != 1 || recs[0].Key != alertKey {
		t.Fatalf("plugin alert lost after crash loop: %+v", recs)
	}

	if err := mgr.Disable(plugintest.ID, "root", "cli"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if n := e.sink.count(alertKey, "recover"); n != 1 {
		t.Fatalf("recover deliveries = %d, want 1", n)
	}
	if recs, _ := e.api.ActiveAlerts(); len(recs) != 0 {
		t.Fatalf("alerts after disable %+v", recs)
	}
	st, _ = mgr.Get(plugintest.ID)
	if st.State != plugin.StateInstalled || st.Running {
		t.Fatalf("after disable %+v", st)
	}
	audit, _ := os.ReadFile(filepath.Join(e.stateDir, "plugins", "audit.jsonl"))
	for _, want := range []string{"plugin.install", "plugin.consent", "plugin.enable", "plugin.config", "plugin.failed", "plugin.disable"} {
		if !strings.Contains(string(audit), `"`+want+`"`) {
			t.Errorf("audit missing %s", want)
		}
	}
}

func TestDaemonWithZeroPluginsUnchanged(t *testing.T) {
	cfg := config.Default()
	if err := cfg.Set("plugins.registry_url", "https://127.0.0.1:9/"); err != nil {
		t.Fatal(err)
	}
	e := startE2EDaemon(t, nil, cfg)
	list, err := e.mgr.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("list %v %v", list, err)
	}
	offers, err := e.mgr.Slots("", plugin.SlotNavPage)
	if err != nil || len(offers) != 0 {
		t.Fatalf("slots %v %v", offers, err)
	}
	recs, err := e.api.ActiveAlerts()
	if err != nil || len(recs) != 0 {
		t.Fatalf("alerts %v %v", recs, err)
	}
	time.Sleep(100 * time.Millisecond)
	e.stop()
	if len(e.sink.alerts) != 0 || len(e.sink.series) != 0 {
		t.Fatalf("zero-plugin daemon produced alerts %v or series %v", e.sink.alerts, e.sink.series)
	}
	var outside []string
	_ = filepath.Walk(e.root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, filepath.Join(e.stateDir, "plugins")+string(filepath.Separator)) {
			outside = append(outside, p)
		}
		return nil
	})
	sort.Strings(outside)
	if len(outside) != 0 {
		t.Fatalf("files written outside <stateDir>/plugins: %v", outside)
	}
}

type rawHostConn struct {
	c  net.Conn
	r  *bufio.Reader
	id int
}

func dialHostRaw(t *testing.T, sock, token string) (*rawHostConn, error) {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	h := &rawHostConn{c: c, r: bufio.NewReader(c)}
	if err := plugin.WriteFrame(c, plugin.Hello{Hello: plugin.ControlHelloMagic, Version: plugin.ControlProtocolVersion, Token: token}); err != nil {
		c.Close()
		return nil, err
	}
	var rep struct {
		Hello string `json:"hello"`
		Error string `json:"error"`
	}
	if err := plugin.ReadFrame(h.r, &rep); err != nil {
		c.Close()
		return nil, err
	}
	if rep.Hello != plugin.ControlHelloMagic {
		c.Close()
		return nil, errors.New("hello refused: " + rep.Error)
	}
	return h, nil
}

func (h *rawHostConn) call(method string, params any) (plugin.Response, error) {
	h.id++
	raw, _ := json.Marshal(params)
	if err := plugin.WriteFrame(h.c, plugin.Request{ID: h.id, Method: method, Params: raw}); err != nil {
		return plugin.Response{}, err
	}
	var resp plugin.Response
	err := plugin.ReadFrame(h.r, &resp)
	return resp, err
}

func TestRuntimeMintedPluginTokenIsConfinedToItsGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a plugin binary")
	}
	pkg := plugintest.Build(t, t.TempDir())
	e := startE2EDaemon(t, pkg.Key, config.Default())
	if _, err := e.mgr.Install(plugin.InstallRequest{Ref: pkg.Path, Actor: "root", Via: "cli"}); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.Enable(plugin.EnableRequest{ID: plugintest.ID, Actor: "root", Via: "cli", Accepted: pkg.Manifest.Permissions}); err != nil {
		t.Fatal(err)
	}
	e2eWait(t, "plugin connected", 30*time.Second, func() bool {
		st, err := e.mgr.Get(plugintest.ID)
		return err == nil && st.Health.Connected
	})
	tokPath := filepath.Join(e.stateDir, "plugins", plugintest.ID, plugin.TokenFileName)
	tb, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(tokPath); info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", info.Mode().Perm())
	}
	tok := strings.TrimSpace(string(tb))
	hostSock := filepath.Join(e.root, "h", "host.sock")
	h, err := dialHostRaw(t, hostSock, tok)
	if err != nil {
		t.Fatalf("plugin token refused on the host socket: %v", err)
	}
	defer h.c.Close()
	if resp, err := h.call(plugin.MethodPublishSeries, plugin.PublishRequest{Metrics: map[string]float64{"other": 1}}); err != nil || !resp.OK {
		t.Fatalf("granted method refused: %+v %v", resp, err)
	}
	for _, m := range []string{plugin.MethodSnapshot, plugin.MethodSeries, plugin.MethodActiveAlerts, plugin.MethodContainers, plugin.MethodFleetNodes, "Plugins.List", "Plugins.Install", "Snapshot", "Config", "AckAlert"} {
		resp, err := h.call(m, struct{}{})
		if err != nil {
			h.c.Close()
			if h, err = dialHostRaw(t, hostSock, tok); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if resp.OK {
			t.Errorf("%s allowed for a plugin token", m)
		}
	}

	e.controlClient(t)
	if _, err := dialHostRaw(t, hostSock, "e2e-control-token"); err == nil {
		t.Error("full-access token accepted on the host socket")
	}
	if _, err := dialHostRaw(t, filepath.Join(e.root, "c.sock"), tok); err == nil {
		t.Error("plugin token accepted on the control socket")
	}

	if err := e.mgr.Disable(plugintest.ID, "root", "cli"); err != nil {
		t.Fatal(err)
	}
	if _, err := dialHostRaw(t, hostSock, tok); err == nil {
		t.Error("token still valid after disable")
	}
	if err := e.mgr.Enable(plugin.EnableRequest{ID: plugintest.ID, Actor: "root", Via: "cli", Accepted: pkg.Manifest.Permissions}); err != nil {
		t.Fatal(err)
	}
	tb2, _ := os.ReadFile(tokPath)
	if strings.TrimSpace(string(tb2)) == tok {
		t.Error("token not rotated on restart")
	}
	e2eWait(t, "plugin reconnected with the new token", 30*time.Second, func() bool {
		st, err := e.mgr.Get(plugintest.ID)
		return err == nil && st.Health.Connected
	})
}
