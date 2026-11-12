package trinetra

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginhost"
)

type hostTestInventory struct {
	mu sync.Mutex
	m  map[string]plugin.Installed
}

func newHostTestInventory(ps ...plugin.Installed) *hostTestInventory {
	inv := &hostTestInventory{m: map[string]plugin.Installed{}}
	for _, p := range ps {
		inv.m[p.ID] = p
	}
	return inv
}

func (i *hostTestInventory) Get(id string) (plugin.Installed, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	p, ok := i.m[id]
	return p, ok
}

func (i *hostTestInventory) Enabled() []plugin.Installed {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []plugin.Installed
	for _, p := range i.m {
		if p.State == plugin.StateEnabled {
			out = append(out, p)
		}
	}
	return out
}

func (i *hostTestInventory) ConfigValues(id string) (plugin.ConfigResponse, error) {
	return plugin.ConfigResponse{Revision: 1, Values: map[string]json.RawMessage{}}, nil
}

func (i *hostTestInventory) Endpoint(id string) (string, string, bool) { return "", "", false }

func enabledInstalled(id string, perms ...plugin.Permission) plugin.Installed {
	return plugin.Installed{ID: id, Version: "1.0.0", State: plugin.StateEnabled, Granted: perms,
		Manifest: plugin.Manifest{ID: id, Permissions: perms}}
}

func TestWireHostSetsRegistryAndUI(t *testing.T) {
	var delivered []Alert
	d := &pluginDeps{
		stateDir:     t.TempDir(),
		inventory:    newHostTestInventory(enabledInstalled("probe", plugin.PermAlertsRaise)),
		deliverAlert: func(a Alert) { delivered = append(delivered, a) },
		submitSeries: func(int64, MetricSet) bool { return true },
		now:          time.Now,
	}
	wireHost(d)
	t.Cleanup(func() {
		for _, s := range d.stops {
			s()
		}
	})
	if d.hostReg == nil || d.ui == nil {
		t.Fatalf("hostReg=%v ui=%v", d.hostReg, d.ui)
	}
	if len(d.starts) != 1 || len(d.stops) != 1 {
		t.Fatalf("starts=%d stops=%d", len(d.starts), len(d.stops))
	}
	d.starts[0]()
	if d.hostReg.HostSocket() != "" {
		t.Fatal("listener started without a host dir")
	}
	if _, err := d.hostReg.Register("probe", []plugin.Permission{plugin.PermAlertsRaise}, false); err != nil {
		t.Fatal(err)
	}
	if pluginAlertBook.Load() == nil {
		t.Fatal("alert book not registered with the inproc API")
	}
	svc := d.hostReg.(*pluginhost.Service)
	if err := svc.Host.RaiseAlert("probe", plugin.AlertRequest{Key: "k", Severity: "critical", Title: "down"}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || delivered[0].Severity != SevCritical || delivered[0].Source != "plugin:probe" ||
		delivered[0].Key != "plugin.probe.k" || delivered[0].Kind != "fire" {
		t.Fatalf("%+v", delivered)
	}
}

func TestWireHostStartsListenerOnHostDir(t *testing.T) {
	d := &pluginDeps{stateDir: t.TempDir(), hostDir: hostapiShortDir(t) + "/run", inventory: newHostTestInventory()}
	wireHost(d)
	d.starts[0]()
	if d.hostReg.HostSocket() == "" {
		t.Fatal("no host socket")
	}
	for _, s := range d.stops {
		s()
	}
	if pluginAlertBook.Load() != nil {
		t.Fatal("alert book still registered after stop")
	}
}

func TestActiveAlertsIncludesPluginAlerts(t *testing.T) {
	d := &pluginDeps{stateDir: t.TempDir(), inventory: newHostTestInventory(enabledInstalled("probe", plugin.PermAlertsRaise))}
	wireHost(d)
	t.Cleanup(func() { d.stops[0]() })
	svc := d.hostReg.(*pluginhost.Service)
	_, _ = svc.Register("probe", []plugin.Permission{plugin.PermAlertsRaise}, false)
	if err := svc.Host.RaiseAlert("probe", plugin.AlertRequest{Key: "check.down", Severity: "warning", Title: "T"}); err != nil {
		t.Fatal(err)
	}
	api := &inprocAPI{stateDir: t.TempDir()}
	recs, err := api.ActiveAlerts()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Key != "plugin.probe.check.down" || recs[0].Source != "plugin:probe" || recs[0].Severity != "warning" {
		t.Fatalf("%+v", recs)
	}
}

func TestAckAlertRoutesPluginKeys(t *testing.T) {
	d := &pluginDeps{stateDir: t.TempDir(), inventory: newHostTestInventory(enabledInstalled("probe", plugin.PermAlertsRaise))}
	wireHost(d)
	t.Cleanup(func() { d.stops[0]() })
	svc := d.hostReg.(*pluginhost.Service)
	_, _ = svc.Register("probe", []plugin.Permission{plugin.PermAlertsRaise}, false)
	_ = svc.Host.RaiseAlert("probe", plugin.AlertRequest{Key: "k", Severity: "info", Title: "T"})
	api := &inprocAPI{stateDir: t.TempDir()}
	if err := api.AckAlert("plugin.probe.k"); err != nil {
		t.Fatal(err)
	}
	recs, _ := api.ActiveAlerts()
	if len(recs) != 1 || !recs[0].Acked {
		t.Fatalf("%+v", recs)
	}
	if err := api.UnackAlert("plugin.probe.k"); err != nil {
		t.Fatal(err)
	}
	if recs, _ = api.ActiveAlerts(); recs[0].Acked {
		t.Fatal("still acked")
	}
	if err := api.AckAlert("plugin.probe.missing"); err == nil {
		t.Fatal("unknown plugin alert acked")
	}
	if err := api.AckAlert("cpu"); err == nil {
		t.Fatal("core key with no active alert acked")
	}
}

func hostapiShortDir(t testing.TB) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "th")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}
