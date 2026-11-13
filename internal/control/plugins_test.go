package control

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeManager struct {
	mu    sync.Mutex
	calls map[string]any
}

func (m *fakeManager) rec(name string, v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == nil {
		m.calls = map[string]any{}
	}
	m.calls[name] = v
}

type idArgs struct{ ID, Actor, Via string }

func (m *fakeManager) List() ([]plugin.Status, error) { m.rec("List", nil); return nil, nil }
func (m *fakeManager) Get(id string) (plugin.Status, error) {
	m.rec("Get", id)
	return plugin.Status{Installed: plugin.Installed{ID: id}}, nil
}
func (m *fakeManager) Install(r plugin.InstallRequest) (plugin.InstallResult, error) {
	m.rec("Install", r)
	return plugin.InstallResult{ID: "x"}, nil
}
func (m *fakeManager) Enable(r plugin.EnableRequest) error { m.rec("Enable", r); return nil }
func (m *fakeManager) Disable(id, a, v string) error       { m.rec("Disable", idArgs{id, a, v}); return nil }
func (m *fakeManager) Remove(id, a, v string) error        { m.rec("Remove", idArgs{id, a, v}); return nil }
func (m *fakeManager) Update(r plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	m.rec("Update", r)
	return nil, nil
}
func (m *fakeManager) SetConfig(r plugin.SetConfigRequest) error { m.rec("SetConfig", r); return nil }
func (m *fakeManager) Config(id string) (plugin.RedactedConfig, error) {
	m.rec("Config", id)
	return plugin.RedactedConfig{Revision: 4}, nil
}
func (m *fakeManager) File(id, name string) (plugin.FileDecl, []byte, error) {
	m.rec("File", [2]string{id, name})
	return plugin.FileDecl{Name: name, ContentType: "text/plain"}, []byte("data"), nil
}
func (m *fakeManager) PackageFile(id string) (plugin.PackageInfo, error) {
	m.rec("PackageFile", id)
	return plugin.PackageInfo{ID: id}, nil
}
func (m *fakeManager) DevAttach(r plugin.DevRequest) (plugin.DevSession, error) {
	m.rec("DevAttach", r)
	return plugin.DevSession{ID: "d"}, nil
}
func (m *fakeManager) DevDetach(id string) error { m.rec("DevDetach", id); return nil }
func (m *fakeManager) Catalog(refresh bool) (plugin.Catalog, error) {
	m.rec("Catalog", refresh)
	return plugin.Catalog{Serial: 1}, nil
}
func (m *fakeManager) Slots(node string, s plugin.Slot) ([]plugin.SlotOffer, error) {
	m.rec("Slots", [2]string{node, string(s)})
	return []plugin.SlotOffer{{PluginID: "p"}}, nil
}
func (m *fakeManager) RenderSlot(r plugin.UIRequest) (plugin.RenderResult, error) {
	m.rec("RenderSlot", r)
	return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetText}}}, nil
}
func (m *fakeManager) InvokeAction(r plugin.UIActionRequest) (plugin.ActionResult, error) {
	m.rec("InvokeAction", r)
	return plugin.ActionResult{OK: true}, nil
}
func (m *fakeManager) Push(r plugin.PushRequest) (plugin.PushResult, error) {
	m.rec("Push", r)
	return plugin.PushResult{}, nil
}
func (m *fakeManager) NodeStates(id string) ([]plugin.NodeState, error) {
	m.rec("NodeStates", id)
	return nil, nil
}

var _ plugin.Manager = (*fakeManager)(nil)

type providerAPI struct {
	*fakeAPI
	mgr plugin.Manager
}

func (p providerAPI) Plugins() plugin.Manager { return p.mgr }

type countListener struct {
	net.Listener
	accepts atomic.Int32
}

func (l *countListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.accepts.Add(1)
	}
	return c, err
}

func serveForTest(t *testing.T, api core.API) (string, *countListener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	cl := &countListener{Listener: ln}
	go func() { _ = Serve(api, cl, "tok") }()
	t.Cleanup(func() { ln.Close() })
	return path, cl
}

func TestPluginsDispatchRoundTrip(t *testing.T) {
	mgr := &fakeManager{}
	path, _ := serveForTest(t, providerAPI{&fakeAPI{}, mgr})
	c, err := Dial(path, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := c.Plugins()

	inst := plugin.InstallRequest{Ref: "demo@1.0.0", Actor: "a", Via: "cli", ExpectSHA256: "ff", RequireVerified: true,
		Consent: &plugin.ConsentRecord{Actor: "a", Permissions: []plugin.Permission{plugin.PermMetricsRead}}}
	en := plugin.EnableRequest{ID: "demo", Actor: "a", Via: "web", Accepted: []plugin.Permission{plugin.PermNetOutbound}, AcknowledgedRisk: true, Pending: true}
	up := plugin.UpdateRequest{ID: "demo", Actor: "a", Via: "cli"}
	sc := plugin.SetConfigRequest{ID: "demo", Values: map[string]string{"k": "v"}, Unset: []string{"z"}, Actor: "a", Via: "web"}
	dev := plugin.DevRequest{Dir: "/tmp/x", Actor: "a"}
	ui := plugin.UIRequest{PluginID: "demo", Node: "n1", Request: plugin.SlotRequest{Slot: plugin.SlotNavPage, SlotID: "main",
		Context: plugin.SlotContext{Container: "c"}, User: plugin.UserContext{Name: "u", Role: plugin.RoleAdmin}}}
	act := plugin.UIActionRequest{PluginID: "demo", Request: plugin.ActionRequest{Action: "go", Params: map[string]string{"a": "b"}, Confirmed: true}}
	push := plugin.PushRequest{ID: "demo", Nodes: []string{"a", "b"}, Tag: "t", Actor: "a", Via: "cli", Remove: true, AcknowledgedRisk: true}

	steps := []struct {
		method string
		call   func() error
		want   any
	}{
		{"List", func() error { _, e := p.List(); return e }, nil},
		{"Get", func() error { _, e := p.Get("demo"); return e }, "demo"},
		{"Install", func() error { _, e := p.Install(inst); return e }, inst},
		{"Enable", func() error { return p.Enable(en) }, en},
		{"Disable", func() error { return p.Disable("demo", "a", "cli") }, idArgs{"demo", "a", "cli"}},
		{"Remove", func() error { return p.Remove("demo", "a", "cli") }, idArgs{"demo", "a", "cli"}},
		{"Update", func() error { _, e := p.Update(up); return e }, up},
		{"SetConfig", func() error { return p.SetConfig(sc) }, sc},
		{"Config", func() error { _, e := p.Config("demo"); return e }, "demo"},
		{"File", func() error {
			d, b, e := p.File("demo", "f.txt")
			if e == nil && (d.Name != "f.txt" || string(b) != "data") {
				t.Errorf("File result %+v %q", d, b)
			}
			return e
		}, [2]string{"demo", "f.txt"}},
		{"PackageFile", func() error { _, e := p.PackageFile("demo"); return e }, "demo"},
		{"DevAttach", func() error { _, e := p.DevAttach(dev); return e }, dev},
		{"DevDetach", func() error { return p.DevDetach("demo") }, "demo"},
		{"Slots", func() error { _, e := p.Slots("n1", plugin.SlotNavPage); return e }, [2]string{"n1", "nav.page"}},
		{"RenderSlot", func() error { _, e := p.RenderSlot(ui); return e }, ui},
		{"InvokeAction", func() error { _, e := p.InvokeAction(act); return e }, act},
		{"Catalog", func() error { _, e := p.Catalog(true); return e }, true},
		{"Push", func() error { _, e := p.Push(push); return e }, push},
		{"NodeStates", func() error { _, e := p.NodeStates("demo"); return e }, "demo"},
	}
	if len(steps) != len(PluginsMethods) {
		t.Fatalf("%d steps for %d methods", len(steps), len(PluginsMethods))
	}
	for _, s := range steps {
		if err := s.call(); err != nil {
			t.Fatalf("%s: %v", s.method, err)
		}
		got := mgr.calls[s.method]
		if !reflect.DeepEqual(got, s.want) {
			t.Errorf("%s: manager saw %#v, want %#v", s.method, got, s.want)
		}
	}
}

func TestPluginsUnavailableWithoutProvider(t *testing.T) {
	path, _ := serveForTest(t, &fakeAPI{})
	c, err := Dial(path, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Plugins().List(); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("List: %v", err)
	}
	if _, err := c.Plugins().RenderSlot(plugin.UIRequest{}); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("RenderSlot: %v", err)
	}
}

func TestPluginsUICallsUseSeparateConnections(t *testing.T) {
	mgr := &fakeManager{}
	path, ln := serveForTest(t, providerAPI{&fakeAPI{}, mgr})
	c, err := Dial(path, "tok")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := ln.accepts.Load()
	p := c.Plugins()
	for i := 0; i < 3; i++ {
		if _, err := p.RenderSlot(plugin.UIRequest{PluginID: "demo"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := ln.accepts.Load() - base; got != 3 {
		t.Fatalf("3 RenderSlot calls opened %d connections", got)
	}
	if _, err := p.List(); err != nil {
		t.Fatal(err)
	}
	if got := ln.accepts.Load() - base; got != 3 {
		t.Fatalf("List opened a new connection (%d)", got)
	}
}
