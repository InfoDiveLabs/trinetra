package pluginrt

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }

type devSession struct {
	rec    plugin.Installed
	token  string
	socket string
}

func (m *Manager) devSession(id string) (devSession, bool) {
	m.rmu.RLock()
	defer m.rmu.RUnlock()
	s, ok := m.dev[id]
	return s, ok
}

// DevAttach implements plugin.Lifecycle.
func (m *Manager) DevAttach(req plugin.DevRequest) (plugin.DevSession, error) {
	if !m.d.AllowUnverified() {
		return plugin.DevSession{}, plugin.ErrUnverifiedDisallowed
	}
	h := m.host()
	if h == nil {
		return plugin.DevSession{}, plugin.ErrUnavailable
	}
	b, err := os.ReadFile(filepath.Join(req.Dir, "plugin.json"))
	if err != nil {
		return plugin.DevSession{}, err
	}
	mf, err := plugin.DecodeManifest(b)
	if err != nil {
		return plugin.DevSession{}, err
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if _, err := m.d.Store.Load(mf.ID); err == nil {
		return plugin.DevSession{}, fmt.Errorf("plugin %s is installed; remove it before attaching a dev build", mf.ID)
	}
	if _, ok := m.devSession(mf.ID); ok {
		return plugin.DevSession{}, fmt.Errorf("plugin %s already has a dev session", mf.ID)
	}
	tok, err := h.Register(mf.ID, mf.Permissions, true)
	if err != nil {
		return plugin.DevSession{}, err
	}
	sock := filepath.Join(os.TempDir(), "trinetra-plugin-dev-"+mf.ID+".sock")
	now := m.now()
	rec := plugin.Installed{ID: mf.ID, Version: mf.Version, Trust: plugin.TrustDev, Manifest: mf, State: plugin.StateEnabled,
		Granted: mf.Permissions, Source: "dev:" + req.Dir, InstalledAt: now, UpdatedAt: now}
	m.rmu.Lock()
	m.dev[mf.ID] = devSession{rec: rec, token: tok, socket: sock}
	m.rmu.Unlock()
	m.audit(req.Actor, "cli", "plugin.install", mf.ID, mf.Version, "dev "+req.Dir)
	return plugin.DevSession{ID: mf.ID, Token: tok, HostSocket: h.HostSocket(), PluginSocket: sock}, nil
}

// DevDetach implements plugin.Lifecycle.
func (m *Manager) DevDetach(id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if _, ok := m.devSession(id); !ok {
		return plugin.ErrNotFound
	}
	if h := m.host(); h != nil {
		h.Unregister(id)
	}
	m.rmu.Lock()
	delete(m.dev, id)
	m.rmu.Unlock()
	return nil
}

// Inventory returns the runtime's read view of installed plugins.
func (m *Manager) Inventory() plugin.Inventory { return inventory{m} }

type inventory struct{ m *Manager }

func (i inventory) Get(id string) (plugin.Installed, bool) { return i.m.installed(id) }
func (i inventory) Enabled() []plugin.Installed            { return i.m.Enabled() }
func (i inventory) ConfigValues(id string) (plugin.ConfigResponse, error) {
	return i.m.ConfigValues(id)
}

func (i inventory) Endpoint(id string) (socket, token string, ok bool) {
	if s, dev := i.m.devSession(id); dev {
		return s.socket, s.token, true
	}
	tok, have := i.m.token(id)
	if !have {
		return "", "", false
	}
	if running, _ := i.m.running(id); !running {
		return "", "", false
	}
	return PluginSocketPath(id), tok, true
}
