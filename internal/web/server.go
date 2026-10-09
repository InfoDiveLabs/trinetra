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

// Deps is what the web server needs from the running daemon, expressed without importing
// internal/trinetra.
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
	// ValidateChannel reports whether a channel config could actually build a working
	// notifier.
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
	// Fleet returns the daemon's core.FleetAPI (fleet-wide status and the node roster) -- the
	// trinetra-web binary's buildDeps wires this to client.Fleet.
	Fleet func() core.FleetAPI
	// StatusPage is the daemon's public status page API (issue #157).
	StatusPage func() core.StatusPageAPI
	// NodeAPI returns a core.API view routed to fleet node id.
	NodeAPI func(id string) core.API
}

// Start is the web server's entry point: given Deps, it binds and serves.
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
