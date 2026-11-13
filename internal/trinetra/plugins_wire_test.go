package trinetra

import (
	"errors"
	"reflect"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestStartPluginsWithStubsServesUnavailable(t *testing.T) {
	mgr, stop := startPlugins(&pluginDeps{})
	if _, err := mgr.List(); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("List: %v", err)
	}
	if _, err := mgr.Catalog(false); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("Catalog: %v", err)
	}
	if _, err := mgr.RenderSlot(plugin.UIRequest{}); !errors.Is(err, plugin.ErrUnavailable) {
		t.Fatalf("RenderSlot: %v", err)
	}
	stop()
	stop()
}

func TestStartPluginsRunsStartsAfterAllWired(t *testing.T) {
	var order []string
	rec := func(s string) func(*pluginDeps) {
		return func(d *pluginDeps) {
			order = append(order, s)
			d.starts = append(d.starts, func() { order = append(order, "start-"+s) })
			d.stops = append(d.stops, func() { order = append(order, "stop-"+s) })
		}
	}
	prevR, prevT, prevH, prevF := wireRegistryFn, wireRuntimeFn, wireHostFn, wireFleetFn
	t.Cleanup(func() { wireRegistryFn, wireRuntimeFn, wireHostFn, wireFleetFn = prevR, prevT, prevH, prevF })
	wireRegistryFn, wireRuntimeFn, wireHostFn, wireFleetFn = rec("registry"), rec("runtime"), rec("host"), rec("fleet")

	_, stop := startPlugins(&pluginDeps{})
	want := []string{"registry", "host", "runtime", "fleet", "start-registry", "start-host", "start-runtime", "start-fleet"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v", order)
	}
	order = nil
	stop()
	wantStop := []string{"stop-fleet", "stop-runtime", "stop-host", "stop-registry"}
	if !reflect.DeepEqual(order, wantStop) {
		t.Fatalf("stop order = %v", order)
	}
}

func TestStartPluginsPassesRegistryAsCatalog(t *testing.T) {
	prev := wireRegistryFn
	t.Cleanup(func() { wireRegistryFn = prev })
	wireRegistryFn = func(d *pluginDeps) { d.registry = fakeRegistry{} }
	mgr, _ := startPlugins(&pluginDeps{})
	c, err := mgr.Catalog(false)
	if err != nil || c.Serial != 42 {
		t.Fatalf("catalog %+v %v", c, err)
	}
}

type fakeRegistry struct{}

func (fakeRegistry) VerifyManifest([]byte, plugin.ManifestSigs) error { return nil }
func (fakeRegistry) Resolve(id, v, goos, arch string) (plugin.PackageRef, error) {
	return plugin.PackageRef{}, nil
}
func (fakeRegistry) Latest(string) (string, bool)             { return "", false }
func (fakeRegistry) Revocation(string, string) (string, bool) { return "", false }
func (fakeRegistry) OnRevoke(func([]plugin.Revocation))       {}
func (fakeRegistry) Catalog(bool) (plugin.Catalog, error)     { return plugin.Catalog{Serial: 42}, nil }

func TestTrySubmitReportsFull(t *testing.T) {
	w := newStoreWriter(nil, 1)
	if !w.trySubmit(storeWrite{ts: 1}) {
		t.Fatal("first submit should queue")
	}
	if w.trySubmit(storeWrite{ts: 2}) {
		t.Fatal("second submit should report a full queue")
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped = %d", w.Dropped())
	}
}

func TestFleetAwareAPIImplementsManagerProvider(t *testing.T) {
	var api core.API = &fleetAwareAPI{}
	prov, ok := api.(plugin.ManagerProvider)
	if !ok {
		t.Fatal("fleetAwareAPI does not implement plugin.ManagerProvider")
	}
	if prov.Plugins() != nil {
		t.Fatal("nil plugins should yield a nil manager")
	}
	mgr, _ := startPlugins(&pluginDeps{})
	api = &fleetAwareAPI{plugins: mgr}
	if api.(plugin.ManagerProvider).Plugins() == nil {
		t.Fatal("manager not exposed")
	}
}
