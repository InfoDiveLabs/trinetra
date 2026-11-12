package trinetra

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type revokeCapturingRegistry struct {
	fakeRegistry
	onRevoke func([]plugin.Revocation)
}

func (r *revokeCapturingRegistry) OnRevoke(f func([]plugin.Revocation)) { r.onRevoke = f }

func newWireMasterDeps(t *testing.T, hub *fleet.Hub, rpc *rpcRegistry) (*pluginDeps, *fleet.Registry) {
	t.Helper()
	reg, err := fleet.OpenRegistry(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	prov := &fleetProvider{role: config.RoleMaster, master: &masterState{reg: reg, hub: hub, sink: &replicaSink{rpc: rpc}}}
	return &pluginDeps{fleet: prov, stateDir: t.TempDir(), now: time.Now, lifecycle: newFleetFakeLifecycle()}, reg
}

func TestWireFleetMasterSetsOps(t *testing.T) {
	d, _ := newWireMasterDeps(t, fleet.NewHub(nil), newRPCRegistry(time.Now, nil))
	d.registry = &revokeCapturingRegistry{}
	t.Cleanup(func() { masterPlugins.p.Store(nil); managedPluginSchema = nil })
	wireFleet(d)
	if d.fleetOps == nil {
		t.Fatal("master left fleetOps nil")
	}
	if masterPlugins.p.Load() == nil {
		t.Fatal("package source not published to the fleet master")
	}
	if managedPluginSchema == nil {
		t.Fatal("managed schema seam not set")
	}
	if d.registry.(*revokeCapturingRegistry).onRevoke == nil {
		t.Fatal("revocation hook not registered")
	}
	mgr := plugin.Compose(d.lifecycle, nil, nil, d.fleetOps)
	if _, err := mgr.Push(plugin.PushRequest{ID: "xp", Nodes: []string{"ghost"}}); err == nil || err == plugin.ErrUnavailable {
		t.Fatalf("Push on a master should reach the pusher, got %v", err)
	}
	for i := len(d.stops) - 1; i >= 0; i-- {
		d.stops[i]()
	}
	if masterPlugins.p.Load() != nil || managedPluginSchema != nil {
		t.Fatal("stop did not clear master hooks")
	}
}

func TestWireFleetSoloLeavesNil(t *testing.T) {
	d := &pluginDeps{fleet: &fleetProvider{role: config.RoleSolo}}
	wireFleet(d)
	if d.fleetOps != nil || len(d.stops) != 0 {
		t.Fatalf("solo wired fleet pieces: %+v", d.fleetOps)
	}
	mgr := plugin.Compose(nil, nil, nil, d.fleetOps)
	if _, err := mgr.Push(plugin.PushRequest{}); err != plugin.ErrUnavailable {
		t.Fatalf("solo Push = %v, want ErrUnavailable", err)
	}
	wireFleet(&pluginDeps{})
}

func TestWireFleetChildPublishesHandlers(t *testing.T) {
	cfg := config.Default()
	d := &pluginDeps{
		fleet:     &fleetProvider{role: config.RoleChild, masterURL: "https://master.example:9443", managed: newManagedChild("", func() *config.Config { return cfg }, nil, nil)},
		stateDir:  t.TempDir(),
		lifecycle: newFleetFakeLifecycle(),
		ui:        &fleetFakeUI{},
		getCfg:    func() *config.Config { return cfg },
	}
	wireFleet(d)
	if activePluginChild.Load() == nil || localPluginUI.Load() == nil {
		t.Fatal("child handlers not published")
	}
	if d.fleet.managed.pluginApply == nil {
		t.Fatal("managed plugin applier not set")
	}
	if got := activePluginChild.Load().via(); got != "fleet:master.example:9443" {
		t.Fatalf("via = %q", got)
	}
	for i := len(d.stops) - 1; i >= 0; i-- {
		d.stops[i]()
	}
	if activePluginChild.Load() != nil || localPluginUI.Load() != nil {
		t.Fatal("stop did not clear child handlers")
	}
}

func TestRevokePropagatesToChildren(t *testing.T) {
	m := newRPCTestMaster(t)
	frames := make(chan fleet.Frame, 4)
	nodeID, _, stop := m.connect("web-1", func(f fleet.Frame) { frames <- f })
	defer stop()

	d, reg := newWireMasterDeps(t, m.hub, m.reg)
	if err := reg.Add(fleet.Node{ID: nodeID, Name: "web-1"}); err != nil {
		t.Fatal(err)
	}
	cap := &revokeCapturingRegistry{}
	d.registry = cap
	t.Cleanup(func() { masterPlugins.p.Store(nil); managedPluginSchema = nil })
	wireFleet(d)

	cap.onRevoke([]plugin.Revocation{{ID: "xp", Versions: []string{"1.0.0"}, Reason: "bad"}})
	select {
	case f := <-frames:
		if f.Type != fleet.FramePluginRevoke {
			t.Fatalf("frame = %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revocation not pushed to the connected child")
	}
}
