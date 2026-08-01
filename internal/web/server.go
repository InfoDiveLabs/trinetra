package web

import (
	"time"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

// sessionGCInterval is how often Start's background sweep removes expired
// records from <StateDir>/sessions.json (session.go's SessionStore.GC).
// Session/ceremony expiry itself is enforced immediately and independently
// by SessionStore.Get treating an expired record as absent (see
// jsonSessionStore.Get) — this ticker only reclaims disk space/file size
// for records nobody ever looks up again after they expire, so an interval
// this coarse costs nothing in correctness.
const sessionGCInterval = 10 * time.Minute

// This package's reach into github.com/go-webauthn/webauthn (what used to
// be pinned here by a placeholder stub, see git history) is now the real
// registration ceremony: webAuthnConfig/beginRegistration/
// finishRegistration in auth_webauthn.go, and *User's webauthn.User
// implementation in users.go (issue #60). This package is compiled into the
// serverwatch-web binary, no build tag.

// Deps is what the web server needs from the running daemon, expressed
// without importing internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go). API is this package's single seam onto
// the daemon's live state (see Deps.API's own doc below): the dashboard,
// monitoring, history/series, and downtime handlers all read through it,
// and its write methods (ApplyConfig/TestChannel/AckAlert/UnackAlert) cover
// config and channel writes too, so this package never needs to import
// serverwatch's own store/config-reload types directly.
type Deps struct {
	// Cfg returns the current config (race-safe against the daemon's reload).
	Cfg func() *config.Config
	// Reload validates, persists, and applies a new config in-process.
	Reload func(*config.Config) error
	// API is the daemon's core.API, the single boundary this package reads
	// live state through (Task 5, core-contract-s1): the dashboard, monitoring,
	// history/series, and downtime handlers all call API.Snapshot()/
	// .Monitoring()/.Series()/.Events() instead of the individual closures/
	// stores those handlers used before this task. May be nil in tests that
	// don't exercise a routed handler; every caller must check before calling
	// (exactly like every other Deps field) and treat a nil API the same as
	// "no data" rather than panicking.
	//
	// As of task 8, the write methods are wired too: configSaveHandler/
	// channelsAddHandler/channelsUpdateHandler/channelsRemoveHandler
	// (handlers_config.go/handlers_channels.go) persist through
	// API.ApplyConfig instead of Deps.Reload, and channelsTestHandler sends
	// through API.TestChannel instead of Deps.TestChannel -- both fields
	// remain on Deps (Deps.Reload still backs the /public settings save,
	// handlers_public.go) but are no longer read by the config/channels
	// paths. AckAlert/UnackAlert are implemented on both core.API backends
	// too, but the alerts page's ack handler (handlers_alerts.go)
	// deliberately keeps its own direct alerts.json read-modify-write: its
	// on-disk shape (ackAlertState) omits ActiveAlert.Critical, which
	// AlertState.Save (the shape AckAlert/UnackAlert round-trip) would
	// preserve -- routing it through API.AckAlert would silently change what
	// gets written, so that page stays on its pre-task-8 path until that
	// divergence is resolved on its own terms.
	API core.API
	// Events is the daemon's downtime event log (EventsStore,
	// events_store.go), backing the alerts page's "Uptime · 30d" tile
	// (uptimePct30d, handlers_alerts.go), the one remaining consumer that
	// isn't routed through API yet (downtimeAPIHandler itself now reads
	// API.Events instead). May be nil (store-writes-disabled mode); callers
	// must treat nil as "no events" rather than assuming it's set.
	Events EventsStore
	// Snapshot returns the latest live snapshot, already projected into this
	// package's own DashboardView (dashboard_view.go) by
	// internal/serverwatch/daemon_web.go's adapter — see that type's doc for
	// why the projection (rather than serverwatch.Snapshot itself) is what
	// crosses this boundary. Lock-free/cheap: safe to call from any
	// goroutine, any number of times. Still used directly by the SSE
	// handlers (sse.go, on every tick), the sidebar nav counts
	// (nav_counts.go), the /public panels (handlers_public.go), and the
	// history page's disk-mount list (historyDiskMounts); dashboardHandler
	// itself now reads through API.Snapshot() instead (Task 5).
	Snapshot func() DashboardView
	// StateDir is the daemon's state directory.
	StateDir string
	// AlertLogPath is the path to the append-only alert log.
	AlertLogPath string
	// AlertStatePath is the path to the alert ack-state file.
	AlertStatePath string
	// TestChannel sends a one-off test notification through the named
	// channel (internal/serverwatch/daemon.go's testChannel closure, built
	// from sendTestNotification/buildNotifier — the same logic `serverwatch
	// channel test <name>` uses). As of task 8, channelsTestHandler
	// (handlers_channels.go) reads through Deps.API.TestChannel instead --
	// this field is kept on Deps (still assigned by daemon_web.go) but no
	// longer read by this package; it stays only in case a future
	// non-core.API consumer needs it directly.
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
// (per cfg.Web.Mode, see serving.go's listenAndServe) when Deps.Enabled and
// returns a stop func that gracefully shuts it down. If Deps.Enabled is
// false, Start binds nothing and returns a no-op stop and a nil error: the
// serverwatch-web binary calls Start unconditionally (see cmd/serverwatch-web),
// so "disabled" has to be a valid, harmless outcome here rather than an error.
//
// When Enabled is true, Start first calls validateOrigin (issue #59) to
// fail fast on a passkey-unsafe or incomplete web.* config BEFORE binding
// anything: a non-nil return here means the caller (serverwatch-web) must
// log it and treat the web server as not started, while it itself
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
