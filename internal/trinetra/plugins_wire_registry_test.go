package trinetra

import (
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/registry"
)

func TestWireRegistrySetsRegistry(t *testing.T) {
	cfg := &config.Config{}
	d := &pluginDeps{stateDir: filepath.Join(t.TempDir(), "state"), getCfg: func() *config.Config { return cfg }}
	wireRegistry(d)
	if d.registry == nil {
		t.Fatal("registry not set")
	}
	if len(d.starts) != 1 || len(d.stops) != 1 {
		t.Fatalf("starts=%d stops=%d", len(d.starts), len(d.stops))
	}
	if _, ok := d.registry.(*registry.Registry); !ok {
		t.Fatalf("registry is %T", d.registry)
	}
	cat, err := d.registry.Catalog(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Keys()) == 0 && cat.Error != plugin.ErrNoRegistryKeys.Error() {
		t.Fatalf("no-keys build should explain: %+v", cat)
	}
	d.stops[0]()
}

func TestWireRegistryInstalledFlagsFollowLifecycle(t *testing.T) {
	d := &pluginDeps{lifecycle: listFake{{ID: "uptime", Version: "1.0.0"}}}
	if got := installedVersions(d); got["uptime"] != "1.0.0" || len(got) != 1 {
		t.Fatalf("installed = %v", got)
	}
}

type listFake []plugin.Installed

func (l listFake) List() ([]plugin.Status, error) {
	out := make([]plugin.Status, len(l))
	for i, in := range l {
		out[i] = plugin.Status{Installed: in}
	}
	return out, nil
}
func (listFake) Get(string) (plugin.Status, error) { return plugin.Status{}, nil }
func (listFake) Install(plugin.InstallRequest) (plugin.InstallResult, error) {
	return plugin.InstallResult{}, nil
}
func (listFake) Enable(plugin.EnableRequest) error                          { return nil }
func (listFake) Disable(string, string, string) error                       { return nil }
func (listFake) Remove(string, string, string) error                        { return nil }
func (listFake) Update(plugin.UpdateRequest) ([]plugin.UpdateResult, error) { return nil, nil }
func (listFake) SetConfig(plugin.SetConfigRequest) error                    { return nil }
func (listFake) Config(string) (plugin.RedactedConfig, error)               { return plugin.RedactedConfig{}, nil }
func (listFake) File(string, string) (plugin.FileDecl, []byte, error) {
	return plugin.FileDecl{}, nil, nil
}
func (listFake) PackageFile(string) (plugin.PackageInfo, error) { return plugin.PackageInfo{}, nil }
func (listFake) DevAttach(plugin.DevRequest) (plugin.DevSession, error) {
	return plugin.DevSession{}, nil
}
func (listFake) DevDetach(string) error { return nil }
