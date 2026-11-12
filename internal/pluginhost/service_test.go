package pluginhost

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type rawHostConn struct {
	conn net.Conn
	r    *bufio.Reader
	n    int
}

func dialHost(t *testing.T, sock, token string) (*rawHostConn, string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &rawHostConn{conn: conn, r: bufio.NewReader(conn)}
	if err := plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.ControlHelloMagic, Version: plugin.ControlProtocolVersion, Token: token}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var resp struct {
		Hello string `json:"hello"`
		Error string `json:"error"`
	}
	if err := plugin.ReadFrame(c.r, &resp); err != nil {
		t.Fatal(err)
	}
	return c, resp.Error
}

func (c *rawHostConn) call(t *testing.T, method string, params any) plugin.Response {
	t.Helper()
	c.n++
	raw, _ := json.Marshal(params)
	if err := plugin.WriteFrame(c.conn, plugin.Request{ID: c.n, Method: method, Params: raw}); err != nil {
		t.Fatal(err)
	}
	var resp plugin.Response
	if err := plugin.ReadFrame(c.r, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func newService(t *testing.T, inv *fakeInv, out *[]AlertOut) *Service {
	t.Helper()
	s, err := NewService(ServiceDeps{
		StateDir: t.TempDir(), Inventory: func() plugin.Inventory { return inv }, Core: &fakeCore{},
		NodeID: "n1", CoreVersion: "v1",
		Submit:  func(int64, map[string]float64) bool { return true },
		Deliver: func(a AlertOut) { *out = append(*out, a) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestServiceListenSocketModes(t *testing.T) {
	var out []AlertOut
	s := newService(t, newFakeInv(), &out)
	dir := filepath.Join(sockDir(t), "run")
	if err := s.Listen(dir); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil || di.Mode().Perm() != 0o750 {
		t.Fatalf("dir %v %v", di, err)
	}
	si, err := os.Stat(filepath.Join(dir, "host.sock"))
	if err != nil || si.Mode().Perm() != 0o660 || si.Mode()&os.ModeSocket == 0 {
		t.Fatalf("sock %v %v", si, err)
	}
	if s.HostSocket() != filepath.Join(dir, "host.sock") {
		t.Fatalf("HostSocket %q", s.HostSocket())
	}
	if err := s.Listen(dir); err != nil {
		t.Fatalf("relisten over stale socket: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRejectsFullAccessTokenOnHostSocket(t *testing.T) {
	var out []AlertOut
	inv := newFakeInv()
	inv.add(enabledPlugin("probe", plugin.PermMetricsPublish))
	s := newService(t, inv, &out)
	dir := sockDir(t)
	if err := s.Listen(dir); err != nil {
		t.Fatal(err)
	}
	sock := s.HostSocket()
	if _, errMsg := dialHost(t, sock, "launch-full-access-token"); errMsg == "" {
		t.Fatal("arbitrary token accepted")
	}
	tok, err := s.Register("probe", []plugin.Permission{plugin.PermMetricsPublish}, false)
	if err != nil {
		t.Fatal(err)
	}
	c, errMsg := dialHost(t, sock, tok)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if r := c.call(t, plugin.MethodHello, plugin.HelloRequest{HostAPI: "1.0"}); !r.OK {
		t.Fatalf("hello: %+v", r)
	}
	if r := c.call(t, plugin.MethodPublishSeries, plugin.PublishRequest{Metrics: map[string]float64{"a": 1}}); !r.OK {
		t.Fatalf("publish: %+v", r)
	}
	for _, m := range []string{"Snapshot", "Plugins.List", "Plugin.Snapshot", "ApplyConfig"} {
		if r := c.call(t, m, struct{}{}); r.OK {
			t.Errorf("%s allowed on plugin token", m)
		}
	}
	h := s.Health("probe")
	if !h.Connected || h.SeriesCount != 1 {
		t.Fatalf("health %+v", h)
	}
	s.Unregister("probe")
	if _, errMsg := dialHost(t, sock, tok); errMsg == "" {
		t.Fatal("token valid after Unregister")
	}
	if s.Health("probe").Connected {
		t.Fatal("still connected after Unregister")
	}
}

func TestServiceRegistryFacade(t *testing.T) {
	var out []AlertOut
	inv := newFakeInv()
	inv.add(enabledPlugin("probe", plugin.PermAlertsRaise))
	s := newService(t, inv, &out)
	if err := s.Host.RaiseAlert("probe", plugin.AlertRequest{Key: "k", Severity: "warning", Title: "t"}); err == nil {
		t.Fatal("raise without registration should be denied")
	}
	_, _ = s.Register("probe", []plugin.Permission{plugin.PermAlertsRaise}, false)
	if err := s.Host.RaiseAlert("probe", plugin.AlertRequest{Key: "k", Severity: "warning", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if len(s.Alerts.Active()) != 1 || len(out) != 1 {
		t.Fatalf("active=%d out=%d", len(s.Alerts.Active()), len(out))
	}
	s.ResolveAll("probe", "disabled")
	if len(s.Alerts.Active()) != 0 || len(out) != 2 || out[1].Kind != "recover" {
		t.Fatalf("%+v", out)
	}
	if fe, err := s.ValidateConfig("probe", nil); err == nil || fe != nil {
		t.Fatalf("validate without endpoint: %v %v", fe, err)
	}
	s.NotifyConfig("probe", plugin.ConfigResponse{Revision: 1})
	s.Unregister("probe")
}
