package web

import (
	"context"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// sessionGCInterval is how often Start's background sweep removes expired records from
// <StateDir>/sessions.json (session.go's SessionStore.GC).
const sessionGCInterval = 10 * time.Minute

// This package's WebAuthn use is the real registration ceremony:
// webAuthnConfig/beginRegistration/finishRegistration in auth_webauthn.go.

// Deps is what the web server needs from the running daemon, expressed
// without importing internal/trinetra (internal/web must never import
// internal/trinetra, to keep the module graph one-way). API is this
// package's single seam onto the daemon's live state (see Deps.API's own
// doc below): the dashboard,
// monitoring, history/series, and downtime handlers all read through it,
// and its write methods (ApplyConfig/TestChannel/AckAlert/UnackAlert) cover
// config and channel writes too, so this package never needs to import
// trinetra's own store/config-reload types directly.
type Deps struct {
	// Cfg returns the current config (race-safe against the daemon's reload).
	Cfg func() *config.Config
	// Reload validates, persists, and applies a new config in-process.
	Reload func(*config.Config) error
	// API is the daemon's core.API, the single boundary this package reads live state through:
	// the dashboard, monitoring, history/series.
	API core.API
	// Events is the daemon's downtime event log (EventsStore, events_store.go), backing the
	// alerts page's "Uptime · 30d" tile (uptimePct30d, handlers_alerts.go).
	Events EventsStore
	// Snapshot returns the latest live snapshot, already projected into this package's own
	// DashboardView.
	Snapshot func() DashboardView
	// StateDir is the web plugin's own state directory (its user store, sessions, and
	// enrollment tokens live here).
	StateDir string
	// TestChannel sends a one-off test notification through the named channel.
	TestChannel func(name string) error
	// ValidateChannel reports whether a channel config could actually build a
	// working notifier (the trinetra-web binary's buildDeps wires it to
	// client.ValidateChannel, which dry-runs trinetra.buildNotifier on the
	// daemon side over the control socket), the same check `channel test` and
	// delivery use, minus the network send.
	// The channels handlers call it before persisting an ENABLED channel so
	// the web editor never silently creates a channel that would be dropped at
	// delivery time (#79 -- e.g. a telegram channel with no chat id). May be
	// nil in tests that don't exercise it; callers must check before calling.
	ValidateChannel func(config.ChannelConfig, *config.Config) error
	// Enabled mirrors cfg.Web.Enabled (the web.enabled config key, issue
	// #58), read once at daemon startup -- see daemon.go's cmdDaemon.
	Enabled bool
	// Listen mirrors cfg.Web.Listen (the web.listen config key, issue #58), a "host:port"
	// string net.SplitHostPort-validated by internal/config.Config.Set.
	Listen string
	// Subscribe opens a live event stream: the daemon's core.API.Subscribe, adapted into this
	// package's own LiveEvent type.
	Subscribe func(context.Context) (<-chan LiveEvent, error)
	// Fleet returns the daemon's core.FleetAPI (fleet-wide status and the
	// node roster) -- the trinetra-web binary's buildDeps wires this to
	// client.Fleet (internal/control, always unrouted: Fleet.* calls run
	// against the master regardless of any node scope). nil means no fleet
	// support at all (e.g. a test Deps that doesn't exercise routing);
	// node_scope.go's fleetRole/withNodeRouter treat a nil Fleet, a nil
	// FleetAPI, or a Status() error identically: "solo", no /n/{node}/...
	// routing. Even a genuinely solo daemon answers Fleet().Status() (role
	// "solo") once wired -- see fleetRole's doc for why every failure mode
	// collapses to that same answer rather than needing separate handling.
	Fleet func() core.FleetAPI
	// StatusPage is the daemon's public status page API (issue #157).
	StatusPage func() core.StatusPageAPI
	// NodeAPI returns a core.API view routed to fleet node id (the
	// trinetra-web binary's buildDeps wires this to
	// func(id string) core.API { return client.ForNode(id) }, internal/
	// control's routed-view client): every core.API method called through
	// it carries that node's id on the wire (see control.Client.ForNode's
	// doc), so it never fails locally for an unknown id -- withNodeRouter
	// validates {node} against Fleet().Nodes(...) before a request ever
	// reaches a handler that would call this. nil means only self is
	// available (no fleet routing wired); node_scope.go's apiFor falls back
	// to d.API in that case, exactly like the self scope.
	NodeAPI func(id string) core.API
}

// Start is the web server's entry point: given Deps, it binds and serves
// (per cfg.Web.Mode, see serving.go's listenAndServe) when Deps.Enabled and
// returns a stop func that gracefully shuts it down. If Deps.Enabled is
// false, Start binds nothing and returns a no-op stop and a nil error: the
// trinetra-web binary calls Start unconditionally (see cmd/trinetra-web),
// so "disabled" has to be a valid, harmless outcome here rather than an error.
//
// When Enabled is true, Start first calls validateOrigin (issue #59) to
// fail fast on a passkey-unsafe or incomplete web.* config BEFORE binding
// anything: a non-nil return here means the caller (trinetra-web) must
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

	// The authenticated-session store, the separate ceremony-placeholder store, and the
	// enrollment-token store.
	sessionGCStop := newSessionStore(d.StateDir).startGC(sessionGCInterval)
	ceremonyGCStop := newCeremonyStore(d.StateDir).startGC(sessionGCInterval)
	tokenGCStop := newTokenStore(d.StateDir).startGC(sessionGCInterval)
	gcStop := func() {
		sessionGCStop()
		ceremonyGCStop()
		tokenGCStop()
	}

	// listenerStop (NOT the named return "stop") is deliberate: the returned closure below
	// calls listenerStop, and if it instead captured "stop" by name.
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
