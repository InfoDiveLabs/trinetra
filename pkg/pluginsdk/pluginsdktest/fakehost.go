// Package pluginsdktest is a fake Trinetra host for testing plugins built on pluginsdk.
package pluginsdktest

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
)

const fakeToken = "fakehost-token"

// FakeHost serves the Plugin.* host API in memory and can call the plugin under test.
type FakeHost struct {
	t       testing.TB
	id      string
	granted []plugin.Permission
	allowed map[string]bool
	dir     string
	hostSck string
	plugSck string
	tokFile string
	ln      net.Listener

	mu        sync.Mutex
	cfg       plugin.ConfigResponse
	published []plugin.PublishRequest
	alerts    []plugin.AlertRequest
	resolved  []plugin.ResolveRequest
	status    plugin.StatusReport
	logs      []plugin.LogEntry
	hellos    int
	snapshot  core.DashboardView
	fleet     []core.NodeSummary
	conts     []core.ContainerView
	contLogs  map[string]string
	series    map[string][]core.SeriesPoint
	active    []core.AlertRecord
	watchers  map[chan plugin.ConfigResponse]struct{}
	cancel    context.CancelFunc
	done      chan struct{}
}

// New starts a fake host for plugin id holding the granted permissions and the initial config values.
func New(t testing.TB, id string, granted []plugin.Permission, config map[string]any) *FakeHost {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pf")
	if err != nil {
		t.Fatal(err)
	}
	h := &FakeHost{
		t: t, id: id, granted: granted, allowed: plugin.AllowedMethods(granted), dir: dir,
		hostSck: filepath.Join(dir, "h.sock"), plugSck: filepath.Join(dir, "p.sock"), tokFile: filepath.Join(dir, "token"),
		watchers: map[chan plugin.ConfigResponse]struct{}{},
	}
	h.cfg = plugin.ConfigResponse{Revision: 1, Values: encode(t, config)}
	if err := os.WriteFile(h.tokFile, []byte(fakeToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", h.hostSck)
	if err != nil {
		t.Fatal(err)
	}
	h.ln = ln
	go h.accept()
	t.Cleanup(h.close)
	return h
}

func encode(t testing.TB, m map[string]any) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

func (h *FakeHost) close() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	h.ln.Close()
	os.RemoveAll(h.dir)
}

// PluginSocket is the socket the plugin under test serves on.
func (h *FakeHost) PluginSocket() string { return h.plugSck }

// Token is the secret the core presents to the plugin.
func (h *FakeHost) Token() string { return fakeToken }

// Start runs p against the fake host in a goroutine and returns once it has connected; it stops on test cleanup.
func (h *FakeHost) Start(p *pluginsdk.Plugin) {
	h.t.Helper()
	env := map[string]string{
		plugin.EnvPluginID: h.id, plugin.EnvHostSocket: h.hostSck, plugin.EnvPluginSocket: h.plugSck,
		plugin.EnvTokenFile: h.tokFile, plugin.EnvDataDir: h.dir,
	}
	p.SetEnv(func(k string) string { return env[k] })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h.mu.Lock()
	h.cancel, h.done = cancel, done
	h.mu.Unlock()
	go func() {
		defer close(done)
		if err := p.Run(ctx); err != nil {
			h.t.Errorf("plugin Run: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ready := h.hellos > 0 && len(h.watchers) > 0
		h.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("pluginsdktest: plugin did not connect to the fake host")
}

// SetConfig replaces the config values and pushes the change to the plugin.
func (h *FakeHost) SetConfig(values map[string]any) {
	h.mu.Lock()
	h.cfg = plugin.ConfigResponse{Revision: h.cfg.Revision + 1, Values: encode(h.t, values)}
	cfg := h.cfg
	for ch := range h.watchers {
		select {
		case ch <- cfg:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- cfg
		}
	}
	h.mu.Unlock()
}

// SetSnapshot sets the view Plugin.Snapshot returns.
func (h *FakeHost) SetSnapshot(v core.DashboardView) {
	h.mu.Lock()
	h.snapshot = v
	h.mu.Unlock()
}

// SetFleetNodes sets the roster Plugin.FleetNodes returns.
func (h *FakeHost) SetFleetNodes(n []core.NodeSummary) {
	h.mu.Lock()
	h.fleet = n
	h.mu.Unlock()
}

// SetContainers sets the list Plugin.Containers returns.
func (h *FakeHost) SetContainers(c []core.ContainerView) {
	h.mu.Lock()
	h.conts = c
	h.mu.Unlock()
}

// SetContainerLogs sets the text Plugin.ContainerLogs returns for name.
func (h *FakeHost) SetContainerLogs(name, logs string) {
	h.mu.Lock()
	if h.contLogs == nil {
		h.contLogs = map[string]string{}
	}
	h.contLogs[name] = logs
	h.mu.Unlock()
}

// SetSeries sets the points Plugin.Series returns for metric.
func (h *FakeHost) SetSeries(metric string, pts []core.SeriesPoint) {
	h.mu.Lock()
	if h.series == nil {
		h.series = map[string][]core.SeriesPoint{}
	}
	h.series[metric] = pts
	h.mu.Unlock()
}

// SetActiveAlerts sets the list Plugin.ActiveAlerts returns.
func (h *FakeHost) SetActiveAlerts(a []core.AlertRecord) {
	h.mu.Lock()
	h.active = a
	h.mu.Unlock()
}

// Published returns every PublishSeries request received.
func (h *FakeHost) Published() []plugin.PublishRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.PublishRequest(nil), h.published...)
}

// Alerts returns the raised alerts in order.
func (h *FakeHost) Alerts() []plugin.AlertRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.AlertRequest(nil), h.alerts...)
}

// Resolved returns the resolve requests in order.
func (h *FakeHost) Resolved() []plugin.ResolveRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.ResolveRequest(nil), h.resolved...)
}

// Status returns the last reported status.
func (h *FakeHost) Status() plugin.StatusReport {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// Logs returns the log lines received.
func (h *FakeHost) Logs() []plugin.LogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.LogEntry(nil), h.logs...)
}

// Render calls the plugin's RenderSlot for slotID as an admin user.
func (h *FakeHost) Render(slotID string, ctx plugin.SlotContext) (plugin.RenderResult, error) {
	var out plugin.RenderResult
	err := h.Call(plugin.CallRenderSlot, plugin.SlotRequest{SlotID: slotID, Context: ctx,
		User: plugin.UserContext{Name: "tester", Role: plugin.RoleAdmin}}, &out)
	return out, err
}

// Invoke calls the plugin's InvokeAction.
func (h *FakeHost) Invoke(req plugin.ActionRequest) (plugin.ActionResult, error) {
	if req.User.Role == "" {
		req.User = plugin.UserContext{Name: "tester", Role: plugin.RoleAdmin}
	}
	var out plugin.ActionResult
	err := h.Call(plugin.CallInvokeAction, req, &out)
	return out, err
}

type reply struct {
	Hello  string          `json:"hello"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Call makes one raw core-to-plugin call over the plugin socket.
func (h *FakeHost) Call(method string, params, result any) error {
	conn, err := net.DialTimeout("unix", h.plugSck, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	if err := plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.PluginHelloMagic, Version: plugin.PluginProtocolVersion, Token: fakeToken}); err != nil {
		return err
	}
	var hr reply
	if err := plugin.ReadFrame(r, &hr); err != nil {
		return err
	}
	if hr.Hello != plugin.PluginHelloMagic {
		return errString(hr.Error)
	}
	raw, _ := json.Marshal(params)
	if err := plugin.WriteFrame(conn, plugin.Request{ID: 1, Method: method, Params: raw}); err != nil {
		return err
	}
	var resp reply
	if err := plugin.ReadFrame(r, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return errString(resp.Error)
	}
	return json.Unmarshal(resp.Result, result)
}

type errString string

func (e errString) Error() string { return string(e) }

func (h *FakeHost) accept() {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			return
		}
		go h.serve(c)
	}
}

func (h *FakeHost) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	var hello plugin.Hello
	if plugin.ReadFrame(r, &hello) != nil {
		return
	}
	if hello.Hello != plugin.ControlHelloMagic || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(fakeToken)) != 1 {
		_ = plugin.WriteFrame(c, plugin.Response{Error: "control: invalid token"})
		return
	}
	_ = plugin.WriteFrame(c, plugin.Hello{Hello: plugin.ControlHelloMagic, Version: plugin.ControlProtocolVersion})
	for {
		var req plugin.Request
		if plugin.ReadFrame(r, &req) != nil {
			return
		}
		if !h.allowed[req.Method] {
			_ = plugin.WriteFrame(c, plugin.Response{ID: req.ID, Error: "control: method \"" + req.Method + "\" not permitted for this token"})
			continue
		}
		if req.Method == plugin.MethodWatchConfig {
			h.stream(c, r, req.ID)
			return
		}
		res, err := h.handle(req)
		resp := plugin.Response{ID: req.ID}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.OK = true
			resp.Result, _ = json.Marshal(res)
		}
		if plugin.WriteFrame(c, resp) != nil {
			return
		}
	}
}

func (h *FakeHost) stream(c net.Conn, r *bufio.Reader, reqID int) {
	ch := make(chan plugin.ConfigResponse, 1)
	h.mu.Lock()
	h.watchers[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.watchers, ch)
		h.mu.Unlock()
	}()
	if plugin.WriteFrame(c, plugin.Response{ID: reqID, OK: true}) != nil {
		return
	}
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, err := r.ReadByte(); err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-gone:
			return
		case cfg := <-ch:
			b, _ := json.Marshal(cfg)
			if plugin.WriteFrame(c, plugin.Response{ID: plugin.StreamID, OK: true, Result: b}) != nil {
				return
			}
		}
	}
}

func decode(req plugin.Request, v any) error {
	if len(req.Params) == 0 {
		return nil
	}
	return json.Unmarshal(req.Params, v)
}

func (h *FakeHost) handle(req plugin.Request) (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch req.Method {
	case plugin.MethodHello:
		var hr plugin.HelloRequest
		if err := decode(req, &hr); err != nil {
			return nil, err
		}
		if err := plugin.HostAPICompatible("^" + hr.HostAPI); err != nil {
			return nil, err
		}
		h.hellos++
		return plugin.HelloResponse{HostAPI: plugin.HostAPIString(), PluginID: h.id, Granted: h.granted, NodeID: "fake-node", TrinetraVersion: "fake"}, nil
	case plugin.MethodGetConfig:
		return h.cfg, nil
	case plugin.MethodSnapshot:
		return h.snapshot, nil
	case plugin.MethodFleetNodes:
		return h.fleet, nil
	case plugin.MethodContainers:
		return h.conts, nil
	case plugin.MethodContainerLogs:
		var r plugin.ContainerLogsRequest
		if err := decode(req, &r); err != nil {
			return nil, err
		}
		return h.contLogs[r.Name], nil
	case plugin.MethodSeries:
		var r plugin.SeriesRequest
		if err := decode(req, &r); err != nil {
			return nil, err
		}
		return h.series[r.Metric], nil
	case plugin.MethodActiveAlerts:
		return h.active, nil
	case plugin.MethodReportStatus:
		var s plugin.StatusReport
		if err := decode(req, &s); err != nil {
			return nil, err
		}
		h.status = s
	case plugin.MethodLog:
		var e plugin.LogEntry
		if err := decode(req, &e); err != nil {
			return nil, err
		}
		h.logs = append(h.logs, e)
	case plugin.MethodPublishSeries:
		var p plugin.PublishRequest
		if err := decode(req, &p); err != nil {
			return nil, err
		}
		h.published = append(h.published, p)
		return plugin.PublishResponse{Accepted: len(p.Metrics)}, nil
	case plugin.MethodRaiseAlert:
		var a plugin.AlertRequest
		if err := decode(req, &a); err != nil {
			return nil, err
		}
		h.alerts = append(h.alerts, a)
	case plugin.MethodResolveAlert:
		var a plugin.ResolveRequest
		if err := decode(req, &a); err != nil {
			return nil, err
		}
		h.resolved = append(h.resolved, a)
	default:
		return nil, errString("pluginsdktest: " + req.Method + " is not implemented by the fake host")
	}
	return struct{}{}, nil
}
