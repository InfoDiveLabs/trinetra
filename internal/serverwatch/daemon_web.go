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
	// events is captured once and reused by both Deps.Events (the /api/
	// downtime and 30d-uptime-tile seam, Task 9) and the Snapshot closure's
	// Availability computation below, rather than calling eventsStoreFor
	// twice -- both just want the same read-only wrapper over d.Store.
	events := eventsStoreFor(d.Store)
	wd := web.Deps{
		Cfg:    d.Cfg,
		Reload: d.Reload,
		Store:  seriesStoreFor(d.Store, d.Cfg),
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
		Monitoring:     func() web.MonitoringView { return buildMonitoringView(d.Snapshot(), d.Cfg()) },
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
// Snapshot. This file (and web.Deps.Snapshot/Monitoring below) just calls
// them, widening core.DashboardView/core.MonitoringView into
// web.DashboardView/web.MonitoringView for free since those are Go type
// aliases (dashboard_view.go/monitoring_view.go).

// seriesStoreFor adapts d.Store (a native serverwatch.SampleStore, possibly
// nil when store-writes-disabled mode leaves the daemon without one) into
// the web.SeriesStore interface (internal/web/series_store.go) — the Task 9
// (#65) resolution of the Task 1 placeholder that made WebDeps.Store widen
// straight into web.Deps.Store as `any`, mirroring buildDashboardView's role
// for Deps.Snapshot.
//
// A nil store returns a true nil web.SeriesStore, NOT a non-nil interface
// wrapping a nil *seriesStoreAdapter: assigning a typed nil pointer into an
// interface value produces a non-nil interface whose method set still
// panics on first use, and internal/web's `d.Store != nil` nil-check
// (handlers_history.go) exists precisely to treat "no store" as "empty
// series" without ever calling into one — so this indirection matters, not
// just style.
func seriesStoreFor(store SampleStore, cfg func() *config.Config) web.SeriesStore {
	if store == nil {
		return nil
	}
	return &seriesStoreAdapter{store: store, cfg: cfg}
}

// eventsStoreFor adapts d.Store (a native serverwatch.SampleStore, possibly
// nil) into the web.EventsStore interface (internal/web/events_store.go),
// backing the history page's "Downtime · 30d" panel — the downtime
// counterpart of seriesStoreFor. It reuses the SAME seriesStoreAdapter
// (which satisfies both web.SeriesStore and web.EventsStore), so the daemon
// hands the web one wrapper for both history feeds. cfg isn't needed for
// Events (no resolution to pick), so it's left nil here.
//
// A nil store returns a true nil web.EventsStore, NOT a non-nil interface
// wrapping a nil *seriesStoreAdapter — same nil-interface gotcha
// seriesStoreFor guards against (see its doc).
func eventsStoreFor(store SampleStore) web.EventsStore {
	if store == nil {
		return nil
	}
	return &seriesStoreAdapter{store: store}
}

// seriesStoreAdapter is the concrete web.SeriesStore/web.EventsStore
// seriesStoreFor/eventsStoreFor build: it wraps a native SampleStore and
// resolves the raw-vs-1m Resolution argument SampleStore.Query needs
// internally (via PickResolution and the daemon's configured
// storage.raw_retention), so internal/web — which holds this only as a
// web.SeriesStore/web.EventsStore — never needs a Resolution type of its own.
type seriesStoreAdapter struct {
	store SampleStore
	// cfg returns the daemon's current config (race-safe against SIGHUP
	// reload, same as WebDeps.Cfg) so Query can read the LIVE
	// storage.raw_retention on every call rather than a value captured once
	// at daemon startup. May be nil in tests that don't care about a
	// specific retention window; Query falls back to defaultRawRetention
	// then, mirroring configuredRawRetention's own nil-safety.
	cfg func() *config.Config
}

// Query implements web.SeriesStore: PickResolution picks raw vs 1m from the
// requested range, "now", and the configured raw retention, then delegates
// to the wrapped SampleStore.Query and widens each returned Point into a
// web.SeriesPoint. A store error is returned as-is (propagated, not
// swallowed) — internal/web's seriesAPIHandler is what decides an error
// still renders as an empty 200 response, not this adapter's job.
func (a *seriesStoreAdapter) Query(metric string, from, to int64) ([]web.SeriesPoint, error) {
	rawRetention := defaultRawRetention
	if a.cfg != nil {
		if cfg := a.cfg(); cfg != nil {
			rawRetention = configuredRawRetention(cfg)
		}
	}
	now := time.Now().Unix()
	res := PickResolution(from, to, now, rawRetention)

	pts, err := a.store.Query(metric, from, to, res)
	if err != nil {
		return nil, err
	}
	out := make([]web.SeriesPoint, len(pts))
	for i, p := range pts {
		out[i] = web.SeriesPoint{TS: p.TS, Min: p.Min, Avg: p.Avg, Max: p.Max}
	}
	return out, nil
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
