package pluginhost

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type fakeInv struct {
	mu        sync.Mutex
	installed map[string]plugin.Installed
	cfg       map[string]plugin.ConfigResponse
	eps       map[string][2]string
}

func newFakeInv() *fakeInv {
	return &fakeInv{installed: map[string]plugin.Installed{}, cfg: map[string]plugin.ConfigResponse{}, eps: map[string][2]string{}}
}

func (f *fakeInv) add(p plugin.Installed) {
	f.mu.Lock()
	f.installed[p.ID] = p
	f.mu.Unlock()
}

func (f *fakeInv) Get(id string) (plugin.Installed, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.installed[id]
	return p, ok
}

func (f *fakeInv) Enabled() []plugin.Installed {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []plugin.Installed
	for _, p := range f.installed {
		if p.State == plugin.StateEnabled {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeInv) ConfigValues(id string) (plugin.ConfigResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cfg[id]
	if !ok {
		return plugin.ConfigResponse{}, errors.New("no config")
	}
	return c, nil
}

func (f *fakeInv) Endpoint(id string) (string, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.eps[id]
	return e[0], e[1], ok
}

func (f *fakeInv) setEndpoint(id, sock, tok string) {
	f.mu.Lock()
	f.eps[id] = [2]string{sock, tok}
	f.mu.Unlock()
}

func enabledPlugin(id string, perms ...plugin.Permission) plugin.Installed {
	return plugin.Installed{ID: id, Version: "1.0.0", State: plugin.StateEnabled, Granted: perms,
		Manifest: plugin.Manifest{ID: id, Permissions: perms, ConfigSchema: &plugin.ConfigSchema{Type: "object",
			Properties: map[string]*plugin.SchemaProp{"n": {Type: "integer", Title: "n", Default: json.RawMessage("7")}}}}}
}

func sockDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type fakeCore struct {
	core.API
	snap       core.DashboardView
	lastMetric string
	lastLines  int
	mon        core.MonitoringView
	alerts     []core.AlertRecord
}

func (c *fakeCore) Snapshot() (core.DashboardView, error) { return c.snap, nil }
func (c *fakeCore) Series(m string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	c.lastMetric = m
	return []core.SeriesPoint{{TS: from}}, nil
}
func (c *fakeCore) Monitoring() (core.MonitoringView, error)      { return c.mon, nil }
func (c *fakeCore) ActiveAlerts() ([]core.AlertRecord, error)     { return c.alerts, nil }
func (c *fakeCore) ContainerLogs(n string, l int) (string, error) { c.lastLines = l; return "log", nil }
