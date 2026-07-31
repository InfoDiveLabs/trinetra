//go:build web

package web

import (
	"time"

	"serverwatch/internal/config"
)

// sessionGCInterval is how often Start's background sweep removes expired
// records from <StateDir>/sessions.json (session.go's SessionStore.GC).
// Session/ceremony expiry itself is enforced immediately and independently
// by SessionStore.Get treating an expired record as absent (see
// jsonSessionStore.Get) — this ticker only reclaims disk space/file size
// for records nobody ever looks up again after they expire, so an interval
// this coarse costs nothing in correctness.
const sessionGCInterval = 10 * time.Minute

// The `-tags web` build's reach into github.com/go-webauthn/webauthn (what
// used to be pinned here by a placeholder stub, see git history) is now the
// real registration ceremony: webAuthnConfig/beginRegistration/
// finishRegistration in auth_webauthn.go, and *User's webauthn.User
// implementation in users.go (issue #60).

// Deps is what the web server needs from the running daemon, expressed
// without importing internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go). Store is this package's own SeriesStore
// interface (series_store.go) — the Task 9 (#65) resolution of the Task 1
// placeholder that made this field `any`, mirroring Task 8's DashboardView
// resolution of Deps.Snapshot: internal/serverwatch/daemon_web.go adapts the
// daemon's concrete SampleStore into a SeriesStore at the call site, so this
// package never needs to import serverwatch's SampleStore/Point/Resolution
// types.
type Deps struct {
	// Cfg returns the current config (race-safe against the daemon's reload).
	Cfg func() *config.Config
	// Reload validates, persists, and applies a new config in-process.
	Reload func(*config.Config) error
	// Store is the daemon's sample store for history queries (SeriesStore,
	// series_store.go). May be nil (e.g. store-writes-disabled mode) —
	// every consumer (seriesAPIHandler, handlers_history.go) must handle
	// that as "no data" rather than assuming it's always set.
	Store SeriesStore
	// Events is the daemon's downtime event log (EventsStore,
	// events_store.go), backing the history page's "Downtime · 30d" panel.
	// Like Store, may be nil (store-writes-disabled mode) — downtimeAPIHandler
	// must treat nil as "no events" rather than assuming it's set.
	Events EventsStore
	// Snapshot returns the latest live snapshot, already projected into this
	// package's own DashboardView (dashboard_view.go) by
	// internal/serverwatch/daemon_web.go's adapter — see that type's doc for
	// why the projection (rather than serverwatch.Snapshot itself) is what
	// crosses this boundary. Lock-free/cheap: safe to call from any
	// goroutine, any number of times (dashboardHandler on every GET /, the
	// SSE handler on every tick).
	Snapshot func() DashboardView
	// Monitoring returns the live snapshot projected into this package's own
	// MonitoringView (monitoring_view.go) by
	// internal/serverwatch/daemon_web.go's buildMonitoringView adapter — the
	// /monitoring detail page's counterpart to Snapshot/DashboardView above.
	// Lock-free/cheap, same contract as Snapshot: safe to call from any
	// goroutine, any number of times. May be nil in tests that don't exercise
	// /monitoring; monitoringHandler (handlers_monitoring.go) must check
	// before calling, exactly like every other Deps func field.
	Monitoring func() MonitoringView
	// StateDir is the daemon's state directory.
	StateDir string
	// AlertLogPath is the path to the append-only alert log.
	AlertLogPath string
	// AlertStatePath is the path to the alert ack-state file.
	AlertStatePath string
	// TestChannel sends a one-off test notification through the named
	// channel (internal/serverwatch/daemon.go's testChannel closure, built
	// from sendTestNotification/buildNotifier — the same logic `serverwatch
	// channel test <name>` uses), for the channels page's "Send test"
	// button (issue #66). May be nil in tests that don't exercise it; every
	// caller (handlers_channels.go) must check before calling.
	TestChannel func(name string) error
	// ValidateChannel reports whether a channel config could actually build a
	// working notifier (daemon_web.go wires it to serverwatch.buildNotifier,
	// the same check `channel test` and delivery use, minus the network send).
	// The channels handlers call it before persisting an ENABLED channel so
	// the web editor never silently creates a channel that would be dropped at
	// delivery time (#79 — e.g. a telegram channel with no chat id). May be
	// nil in tests that don't exercise it; callers must check before calling.
	ValidateChannel func(config.ChannelConfig, *config.Config) error
	// Enabled mirrors cfg.Web.Enabled (the web.enabled config key, issue
	// #58), read once at daemon startup — see daemon.go's cmdDaemon.
	Enabled bool
	// Listen mirrors cfg.Web.Listen (the web.listen config key, issue #58),
	// a "host:port" string net.SplitHostPort-validated by
	// internal/config.Config.Set.
	Listen string
}

// Start is the web server's entry point: given Deps, it binds and serves
// (per cfg.Web.Mode — see serving.go's listenAndServe) when Deps.Enabled and
// returns a stop func that gracefully shuts it down. If Deps.Enabled is
// false, Start binds nothing and returns a no-op stop and a nil error — the
// daemon always calls maybeStartWeb/Start unconditionally (see
// internal/serverwatch/web_deps.go), so "disabled" has to be a valid,
// harmless outcome here rather than an error.
//
// When Enabled is true, Start first calls validateOrigin (issue #59) to
// fail fast on a passkey-unsafe or incomplete web.* config BEFORE binding
// anything: a non-nil return here means the caller (maybeStartWeb) must
// log it and treat the web server as not started, while the daemon itself
// keeps running.
func Start(d Deps) (stop func(), err error) {
	if !d.Enabled {
		return func() {}, nil
	}
	cfg := d.Cfg()
	if err := validateOrigin(cfg); err != nil {
		return nil, err
	}

	// The authenticated-session store, the separate ceremony-placeholder
	// store, and the enrollment-token store (session.go/enroll_tokens.go)
	// all need a periodic GC sweep independent of any particular request —
	// see sessionGCInterval's doc — so all three are started once here,
	// alongside the listener, rather than per-request like newHandler's
	// other per-call newSessionStore/newCeremonyStore/newTokenStore uses.
	sessionGCStop := newSessionStore(d.StateDir).startGC(sessionGCInterval)
	ceremonyGCStop := newCeremonyStore(d.StateDir).startGC(sessionGCInterval)
	tokenGCStop := newTokenStore(d.StateDir).startGC(sessionGCInterval)
	gcStop := func() {
		sessionGCStop()
		ceremonyGCStop()
		tokenGCStop()
	}

	// listenerStop (NOT the named return "stop") is deliberate: the returned
	// closure below calls listenerStop, and if it instead captured "stop" by
	// name, assigning the closure itself to "stop" via `return func(){...}`
	// would make the closure call itself — infinite recursion.
	listenerStop, err := listenAndServe(d, newHandler(d))
	if err != nil {
		gcStop()
		return nil, err
	}
	return func() {
		listenerStop()
		gcStop()
	}, nil
}
