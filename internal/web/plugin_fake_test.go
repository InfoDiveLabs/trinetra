package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeManager struct {
	mu sync.Mutex

	statuses map[string]plugin.Status
	configs  map[string]plugin.RedactedConfig
	catalog  plugin.Catalog
	catErr   error
	offers   []plugin.SlotOffer
	files    map[string][]byte
	nodes    []plugin.NodeState

	renderFn func(plugin.UIRequest) (plugin.RenderResult, error)
	actionFn func(plugin.UIActionRequest) (plugin.ActionResult, error)
	delay    map[string]time.Duration

	installed []plugin.InstallRequest
	enabled   []plugin.EnableRequest
	disabled  []string
	removed   []string
	updated   []plugin.UpdateRequest
	setCfg    []plugin.SetConfigRequest
	pushed    []plugin.PushRequest
	renders   []plugin.UIRequest
	actions   []plugin.UIActionRequest
	slotNodes []string
	refreshed int

	installErr error
	enableErr  error
	setCfgErr  error
	updateRes  []plugin.UpdateResult
}

var _ plugin.Manager = (*fakeManager)(nil)

func newFakeManager(sts ...plugin.Status) *fakeManager {
	f := &fakeManager{statuses: map[string]plugin.Status{}, configs: map[string]plugin.RedactedConfig{}, files: map[string][]byte{}, delay: map[string]time.Duration{}}
	for _, s := range sts {
		f.statuses[s.ID] = s
	}
	return f
}

func (f *fakeManager) List() ([]plugin.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []plugin.Status
	for _, s := range f.statuses {
		out = append(out, s)
	}
	sortStatuses(out)
	return out, nil
}

func (f *fakeManager) Get(id string) (plugin.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.statuses[id]
	if !ok {
		return plugin.Status{}, plugin.ErrNotFound
	}
	return s, nil
}

func (f *fakeManager) Install(r plugin.InstallRequest) (plugin.InstallResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, r)
	if f.installErr != nil {
		return plugin.InstallResult{}, f.installErr
	}
	return plugin.InstallResult{ID: "demo", Version: "1.0.0"}, nil
}

func (f *fakeManager) Enable(r plugin.EnableRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enabled = append(f.enabled, r)
	return f.enableErr
}

func (f *fakeManager) Disable(id, actor, via string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disabled = append(f.disabled, id)
	return nil
}

func (f *fakeManager) Remove(id, actor, via string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeManager) Update(r plugin.UpdateRequest) ([]plugin.UpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updated = append(f.updated, r)
	return f.updateRes, nil
}

func (f *fakeManager) SetConfig(r plugin.SetConfigRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCfg = append(f.setCfg, r)
	return f.setCfgErr
}

func (f *fakeManager) Config(id string) (plugin.RedactedConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configs[id], nil
}

func (f *fakeManager) File(id, name string) (plugin.FileDecl, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.statuses[id]
	if !ok {
		return plugin.FileDecl{}, nil, plugin.ErrNotFound
	}
	for _, fd := range st.Manifest.Contributes.Files {
		if fd.Name == name {
			return fd, f.files[id+"/"+name], nil
		}
	}
	return plugin.FileDecl{}, nil, plugin.ErrNotFound
}

func (f *fakeManager) PackageFile(id string) (plugin.PackageInfo, error) {
	return plugin.PackageInfo{}, plugin.ErrUnavailable
}

func (f *fakeManager) DevAttach(plugin.DevRequest) (plugin.DevSession, error) {
	return plugin.DevSession{}, plugin.ErrUnavailable
}

func (f *fakeManager) DevDetach(string) error { return nil }

func (f *fakeManager) Catalog(refresh bool) (plugin.Catalog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if refresh {
		f.refreshed++
	}
	return f.catalog, f.catErr
}

func (f *fakeManager) Slots(node string, slot plugin.Slot) ([]plugin.SlotOffer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slotNodes = append(f.slotNodes, node)
	var out []plugin.SlotOffer
	for _, o := range f.offers {
		if o.Slot == slot {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeManager) RenderSlot(r plugin.UIRequest) (plugin.RenderResult, error) {
	f.mu.Lock()
	f.renders = append(f.renders, r)
	fn := f.renderFn
	d := f.delay[r.PluginID+"/"+r.Request.SlotID]
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if fn == nil {
		return plugin.RenderResult{}, nil
	}
	return fn(r)
}

func (f *fakeManager) InvokeAction(r plugin.UIActionRequest) (plugin.ActionResult, error) {
	f.mu.Lock()
	f.actions = append(f.actions, r)
	fn := f.actionFn
	f.mu.Unlock()
	if fn == nil {
		return plugin.ActionResult{OK: true, Message: "done"}, nil
	}
	return fn(r)
}

func (f *fakeManager) Push(r plugin.PushRequest) (plugin.PushResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, r)
	return plugin.PushResult{}, nil
}

func (f *fakeManager) NodeStates(id string) ([]plugin.NodeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nodes, nil
}

func sortStatuses(s []plugin.Status) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].ID < s[j-1].ID; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func demoStatus() plugin.Status {
	var st plugin.Status
	st.ID = "demo"
	st.Version = "1.2.0"
	st.Trust = plugin.TrustVerified
	st.State = plugin.StateEnabled
	st.Running = true
	st.Manifest = plugin.Manifest{
		ID: "demo", Name: "Demo Plugin", Version: "1.2.0", Publisher: "Acme",
		Permissions: []plugin.Permission{plugin.PermMetricsPublish},
		Contributes: plugin.Contributes{
			Slots: []plugin.SlotDecl{
				{Slot: plugin.SlotNavPage, ID: "main", Title: "Demo", MinRole: plugin.RoleViewer},
				{Slot: plugin.SlotDashboardPanel, ID: "panel", Title: "Demo panel", MinRole: plugin.RoleViewer},
			},
			Actions: []plugin.ActionDecl{
				{ID: "restart", Label: "Restart it", Confirm: "Really restart?", MinRole: plugin.RoleAdmin, Mutating: true},
				{ID: "ping", Label: "Ping", MinRole: plugin.RoleViewer},
			},
			Files: []plugin.FileDecl{{Name: "report.csv", Title: "Report", ContentType: "text/csv"}},
		},
	}
	st.Granted = []plugin.Permission{plugin.PermMetricsPublish}
	return st
}

type pluginEnv struct {
	t        *testing.T
	d        Deps
	h        http.Handler
	m        *fakeManager
	users    UserStore
	sessions SessionStore
}

func newPluginEnv(t *testing.T, m *fakeManager) *pluginEnv {
	t.Helper()
	d := enrollTestDeps(t)
	if m != nil {
		d.Plugins = func() plugin.Manager { return m }
	}
	return &pluginEnv{t: t, d: d, h: newHandler(d), m: m, users: newUserStore(d.StateDir), sessions: newSessionStore(d.StateDir)}
}

func (e *pluginEnv) get(role Role, target string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := seedSignedInRequest(e.t, e.users, e.sessions, role, http.MethodGet, target)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}

func (e *pluginEnv) post(role Role, target string, form map[string][]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := seedSignedInRequest(e.t, e.users, e.sessions, role, http.MethodGet, "/")
	c := req.Cookies()[0]
	sess, _ := e.sessions.Get(c.Value)
	vals := map[string][]string{"csrf_token": {sess.CSRF}}
	for k, v := range form {
		vals[k] = v
	}
	return e.postRaw(c, target, vals)
}

func (e *pluginEnv) postRaw(c *http.Cookie, target string, vals map[string][]string) *httptest.ResponseRecorder {
	e.t.Helper()
	body := url.Values(vals).Encode()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(c)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr
}
