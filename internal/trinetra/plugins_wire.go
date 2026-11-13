package trinetra

import (
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const (
	pluginLibDir  = "/usr/local/lib/trinetra/plugins"
	pluginHostDir = "/run/trinetra-plugins"
)

// pluginDeps carries the daemon's facilities to each plugin track's wiring and collects the parts they build.
type pluginDeps struct {
	stateDir     string
	libDir       string
	hostDir      string
	getCfg       func() *config.Config
	self         core.API
	fleet        *fleetProvider
	submitSeries func(ts int64, ms MetricSet) bool
	deliverAlert func(a Alert)
	logf         func(format string, args ...any)
	now          func() time.Time
	coreVersion  string

	registry  plugin.Registry
	lifecycle plugin.Lifecycle
	inventory plugin.Inventory
	hostReg   plugin.HostRegistry
	ui        plugin.UI
	fleetOps  plugin.FleetOps

	starts []func()
	stops  []func()
}

var (
	wireRegistryFn = wireRegistry
	wireRuntimeFn  = wireRuntime
	wireHostFn     = wireHost
	wireFleetFn    = wireFleet
)

// startPlugins wires registry, hostapi, runtime, fleet (in that order) so the host socket listens before plugins start, runs starts, and returns the Manager and a stop func (stops in reverse).
func startPlugins(d *pluginDeps) (plugin.Manager, func()) {
	wireRegistryFn(d)
	wireHostFn(d)
	wireRuntimeFn(d)
	wireFleetFn(d)
	var catalog plugin.CatalogSource
	if d.registry != nil {
		catalog = d.registry
	}
	mgr := plugin.Compose(d.lifecycle, d.ui, catalog, d.fleetOps)
	for _, f := range d.starts {
		f()
	}
	stopped := false
	return mgr, func() {
		if stopped {
			return
		}
		stopped = true
		for i := len(d.stops) - 1; i >= 0; i-- {
			d.stops[i]()
		}
	}
}
