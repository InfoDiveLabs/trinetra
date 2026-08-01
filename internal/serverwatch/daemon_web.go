//go:build web

package serverwatch

import (
	"fmt"
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
	"serverwatch/internal/web"
)

// maybeStartWeb is the `-tags web` half of the seam: it is the ONLY file in
// this package allowed to import internal/web (see web_deps.go's design
// note). It adapts serverwatch's native WebDeps into web.Deps — a
// serverwatch-free type — and delegates to web.Start.
//
// web.Start can legitimately fail even when Enabled is true (issue #59's
// validateOrigin rejecting a passkey-unsafe or incomplete web.* config)
// before it ever binds a listener, in which case it returns a nil stop and
// a non-nil error. cmdDaemon (daemon.go) unconditionally defers whatever
// stop func this returns, so passing that nil straight through would panic
// the daemon on shutdown; instead, log the failure to stderr and hand back
// a safe no-op so the daemon keeps running without the web UI.
func maybeStartWeb(d WebDeps) func() {
	// events is captured once and reused by both Deps.Events (the alerts
	// page's 30d-uptime-tile seam, uptimePct30d -- the one Events consumer
	// core.API doesn't cover yet) and the Snapshot closure's Availability
	// computation below, rather than calling eventsStoreFor twice -- both
	// just want the same read-only wrapper over d.Store.
	events := eventsStoreFor(d.Store)
	wd := web.Deps{
		Cfg:    d.Cfg,
		Reload: d.Reload,
		// API is the in-process core.API implementation (Task 5,
		// core-contract-s1): the dashboard/monitoring/history/downtime
		// handlers read live daemon state through this instead of the
		// individual Store/Monitoring closures those handlers used before
		// this task (see web.Deps.API's doc). getSnap/getCfg are the exact
		// same closures WebDeps already hands this build (d.Snapshot/d.Cfg);
		// only the constructor differs from the default (`!web`) build's use
		// of the same newInprocAPI (coreapi_inproc.go). d.Reload is passed
		// through unchanged as ApplyConfig's backing closure (task 8).
		API:    newInprocAPI(d.Snapshot, d.Cfg, d.Store, d.StateDir, d.Reload),
		Events: events,
		Snapshot: func() web.DashboardView {
			v := buildDashboardView(d.Snapshot())
			// Availability is computed fresh on every Snapshot() call (real
			// "now", real events overlapping the trailing 24h) rather than
			// baked into buildDashboardView, since that function's existing
			// unit tests (daemon_web_dashboard_test.go) exercise it against
			// a bare Snapshot with no events store in scope -- see
			// ComputeAvailability's doc (internal/core/availability.go) for
			// why a nil/erroring store just degrades to "no events".
			v.Availability = core.ComputeAvailability(events, time.Now().Unix())
			return v
		},
		StateDir:       d.StateDir,
		AlertLogPath:   d.AlertLogPath,
		AlertStatePath: d.AlertStatePath,
		TestChannel:    d.TestChannel,
		ValidateChannel: func(cc config.ChannelConfig, c *config.Config) error {
			// Same check `channel test` and delivery use, minus the network
			// send: does this channel build a working notifier? (#79)
			_, err := buildNotifier(cc, c)
			return err
		},
		Enabled: d.Enabled,
		Listen:  d.Listen,
	}
	stop, err := web.Start(wd)
	if err != nil {
		fmt.Fprintln(stderr, "web: failed to start, web UI disabled:", err)
		return func() {}
	}
	return stop
}

// buildDashboardView/buildMonitoringView (the Snapshot -> core.DashboardView/
// core.MonitoringView projections) and their helpers moved to the untagged
// coreapi_inproc.go, so both this `-tags web` build AND the default build
// (the in-process core.API, newInprocAPI) can build views from a live
// Snapshot. This file's Snapshot closure above calls buildDashboardView
// directly (widening core.DashboardView into web.DashboardView for free,
// since those are Go type aliases -- dashboard_view.go); buildMonitoringView
// is reached the same way, but only through wd.API (newInprocAPI) now that
// the /monitoring page reads through core.API (Task 5) rather than a
// separate Deps.Monitoring closure.

// eventsStoreFor adapts d.Store (a native serverwatch.SampleStore, possibly
// nil) into the web.EventsStore interface (internal/web/events_store.go),
// backing the history page's "Downtime · 30d" panel and the alerts page's
// "Uptime · 30d" tile (Deps.Events). It wraps the store in a
// seriesStoreAdapter, the same adapter type the now-removed seriesStoreFor
// used to build for the read/Query side (core.API.Series has covered that
// path directly since Task 5, core-contract-s1). Events is the one method
// on that adapter still live in production.
//
// A nil store returns a true nil web.EventsStore, NOT a non-nil interface
// wrapping a nil *seriesStoreAdapter: assigning a typed nil pointer into an
// interface value produces a non-nil interface whose method set still
// panics on first use, so this indirection matters, not just style.
func eventsStoreFor(store SampleStore) web.EventsStore {
	if store == nil {
		return nil
	}
	return &seriesStoreAdapter{store: store}
}

// seriesStoreAdapter is the concrete web.EventsStore eventsStoreFor builds:
// it wraps a native SampleStore. Its cfg field and the raw-vs-1m Resolution
// handling it used to support belonged to the adapter's former Query method
// (the web.SeriesStore side, removed once core.API.Series took over history
// reads); Events, the method still in production use, needs no resolution
// picking, so cfg goes unused on that path.
type seriesStoreAdapter struct {
	store SampleStore
	// cfg returns the daemon's current config (race-safe against SIGHUP
	// reload, same as WebDeps.Cfg). Unused by Events; kept on the struct
	// only because tests still construct this adapter with it set. May be
	// nil.
	cfg func() *config.Config
}

// Events implements web.EventsStore: it delegates to the wrapped
// SampleStore.Events (downtime events overlapping [from, to]) and widens
// each native DownEvent into a web.DownEventView. A store error is returned
// as-is — internal/web's downtimeAPIHandler decides an error still renders
// as an empty 200, not this adapter's job.
func (a *seriesStoreAdapter) Events(from, to int64) ([]web.DownEventView, error) {
	evs, err := a.store.Events(from, to)
	if err != nil {
		return nil, err
	}
	out := make([]web.DownEventView, len(evs))
	for i, e := range evs {
		out[i] = web.DownEventView{Type: e.Type, Start: e.Start, End: e.End, DurationSec: e.DurationSec}
	}
	return out, nil
}
