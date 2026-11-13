package trinetra

import (
	"path/filepath"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// pluginFleetOps is the master's plugin.FleetOps: push orchestration plus relayed UI.
type pluginFleetOps struct {
	*pluginPusher
	*pluginRelay
}

func pluginMasterIdentity(masterURL string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(masterURL, "https://"), "http://")
	return strings.TrimSuffix(s, "/")
}

func wireFleet(d *pluginDeps) {
	if d.fleet == nil {
		return
	}
	lifecycle := func() plugin.Lifecycle { return d.lifecycle }
	switch d.fleet.role {
	case config.RoleMaster:
		wireFleetMaster(d, lifecycle)
	case config.RoleChild:
		wireFleetChild(d, lifecycle)
	}
}

func wireFleetMaster(d *pluginDeps, lifecycle func() plugin.Lifecycle) {
	m := d.fleet.master
	if m == nil {
		return
	}
	configFor := func(id string) map[string]string {
		if d.inventory == nil {
			return nil
		}
		cfg, err := d.inventory.ConfigValues(id)
		if err != nil {
			return nil
		}
		return configStrings(cfg.Values)
	}
	pusher := newPluginPusher(lifecycle, m.reg.List, m.hub.Push, m.hub.Connected, m.audit, configFor, d.now)
	relay := &pluginRelay{hub: m.hub, rpc: m.sink.rpc, resolve: nodeResolver(m.reg.List)}
	d.fleetOps = &pluginFleetOps{pluginPusher: pusher, pluginRelay: relay}
	masterPlugins.p.Store(pusher)
	managedPluginSchema = func(id string) (*plugin.ConfigSchema, bool) {
		lc := lifecycle()
		if lc == nil {
			return nil, false
		}
		st, err := lc.Get(id)
		if err != nil {
			return nil, false
		}
		return st.Manifest.ConfigSchema, true
	}
	if d.registry != nil {
		d.registry.OnRevoke(pusher.Revoke)
	}
	d.stops = append(d.stops, func() {
		masterPlugins.p.Store(nil)
		managedPluginSchema = nil
	})
}

func wireFleetChild(d *pluginDeps, lifecycle func() plugin.Lifecycle) {
	masterID := pluginMasterIdentity(d.fleet.masterURL)
	registry := func() plugin.Registry { return d.registry }
	allow := func() bool { return d.getCfg == nil || d.getCfg() == nil || d.getCfg().PluginsAllowUnverified() }
	pc := newPluginChild(lifecycle, registry, allow, masterID, filepath.Join(d.stateDir, "plugin-fleet-dl"), d.logf)
	activePluginChild.Store(pc)
	if d.ui != nil {
		setLocalPluginUI(d.ui)
	}
	if d.fleet.managed != nil {
		d.fleet.managed.SetPluginApplier(newManagedPluginApplier(lifecycle, "fleet:"+masterID))
	}
	d.stops = append(d.stops, func() {
		activePluginChild.Store(nil)
		setLocalPluginUI(nil)
		if d.fleet.managed != nil {
			d.fleet.managed.SetPluginApplier(nil)
		}
	})
}
