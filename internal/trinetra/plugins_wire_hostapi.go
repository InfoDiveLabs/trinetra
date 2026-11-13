package trinetra

import (
	"sync/atomic"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginhost"
)

// pluginAlertBook is the live plugin alert store consulted by the inproc core API.
var pluginAlertBook atomic.Pointer[pluginhost.AlertBook]

func alertFromPlugin(a pluginhost.AlertOut) Alert {
	sev, err := ParseSeverity(a.Severity)
	if err != nil {
		sev = SevWarning
	}
	return Alert{Key: a.Key, Title: a.Title, Body: a.Body, Severity: sev, Kind: a.Kind, Source: a.Source, Time: a.Time}
}

func wireHost(d *pluginDeps) {
	logf := d.logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	nodeID := core.SelfNodeID
	var fleetFn func() core.FleetAPI
	if d.fleet != nil {
		if d.fleet.nodeID != "" {
			nodeID = d.fleet.nodeID
		}
		fleetFn = d.fleet.Fleet
	}
	var submit func(int64, map[string]float64) bool
	if d.submitSeries != nil {
		submit = func(ts int64, ms map[string]float64) bool { return d.submitSeries(ts, MetricSet(ms)) }
	}
	svc, err := pluginhost.NewService(pluginhost.ServiceDeps{
		StateDir:    d.stateDir,
		Inventory:   func() plugin.Inventory { return d.inventory },
		Core:        d.self,
		Fleet:       fleetFn,
		NodeID:      nodeID,
		CoreVersion: d.coreVersion,
		Submit:      submit,
		Deliver: func(a pluginhost.AlertOut) {
			if d.deliverAlert != nil {
				d.deliverAlert(alertFromPlugin(a))
			}
		},
		Logf: logf,
		Now:  d.now,
	})
	if err != nil {
		logf("plugins: host API unavailable: %v", err)
		return
	}
	d.hostReg = svc
	d.ui = svc.UI
	pluginAlertBook.Store(svc.Alerts)
	d.starts = append(d.starts, func() {
		if d.hostDir == "" {
			return
		}
		if pluginIsRoot() && !hasSystemd() {
			if err := pluginEnsureUser(); err != nil {
				logf("plugins: %v", err)
			}
		}
		if err := svc.Listen(d.hostDir); err != nil {
			logf("plugins: host socket: %v", err)
		}
	})
	d.stops = append(d.stops, func() {
		_ = svc.Close()
		pluginAlertBook.CompareAndSwap(svc.Alerts, nil)
	})
}
