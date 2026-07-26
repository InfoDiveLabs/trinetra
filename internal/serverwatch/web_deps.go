package serverwatch

import (
	"sync/atomic"

	"serverwatch/internal/config"
)

// Seam design decision (Task 1 / issue #57 — see the epic's design doc,
// ui-webapp-design.md, "Release model"/"Architecture" sections):
//
// The default `serverwatch` build must stay 100% stdlib. `internal/web`
// (compiled only with `-tags web`) hosts the UI and pulls in third-party
// deps (go-webauthn, x/crypto/autocert). The two packages are wired together
// through exactly two build-tagged files in THIS package:
//
//	daemon_web.go   (//go:build web)   imports "serverwatch/internal/web"
//	daemon_noweb.go (//go:build !web)  no-op, no web import
//
// Both define maybeStartWeb(WebDeps) func(), which cmdDaemon (daemon.go,
// untagged) always calls — so daemon.go itself never has to know which
// variant is linked in.
//
// The one constraint that makes this safe is DIRECTIONAL: internal/web must
// never import internal/serverwatch. If it did, then building with -tags web
// would give the compiler this graph:
//
//	serverwatch (package, via daemon_web.go) -> internal/web -> serverwatch
//
// — a direct import cycle, fatal at compile time, regardless of build tags
// (tags gate which FILES are compiled, not whether a resulting cycle between
// the two PACKAGES is allowed once those files are in). So:
//
//   - WebDeps is defined HERE (untagged), using serverwatch's own native
//     Snapshot/SampleStore types freely — that's an intra-package reference,
//     not a cross-package import, so it can't create a cycle.
//   - internal/web's exported Deps/Start, however, do NOT reference
//     serverwatch.WebDeps, serverwatch.Snapshot, or serverwatch.SampleStore.
//     internal/web only imports serverwatch/internal/config (a leaf package
//     imported by neither serverwatch's web files nor web itself in a way
//     that cycles) plus stdlib.
//   - daemon_web.go — which, being part of package serverwatch, may
//     freely use serverwatch's native types AND import internal/web — is the
//     only place that adapts a WebDeps into whatever shape internal/web
//     expects at its current stage. In this task that shape is trivial
//     (Store is `any`, Snapshot is `func() any`) because web.Start is still a
//     stub; later tasks (w8 dashboard/SSE, w9 history) will define proper
//     web-local interfaces for the snapshot/store shapes web actually needs,
//     and daemon_web.go will adapt serverwatch's concrete types into those
//     instead of widening WebDeps itself.
//
// Net effect: the module graph is one-way, serverwatch -> web, never the
// reverse, in both the tagged and untagged build.

// WebDeps hands a future internal/web server exactly what it needs from the
// running daemon: race-safe config access/mutation, the sample store, the
// latest merged snapshot, state-dir paths, and the web.enabled/listen
// settings. It is deliberately built from primitives, func values, and
// serverwatch's own SampleStore/Snapshot types (see the design note above) —
// never a serverwatch type that would force internal/web to import this
// package.
type WebDeps struct {
	// Cfg returns the current config, race-safe against the daemon's SIGHUP
	// reload (mirrors cmdDaemon's getCfg closure).
	Cfg func() *config.Config
	// Reload validates, persists, and applies a new config in-process: the
	// same swap-cfg-and-rebuild-dispatcher step the SIGHUP handler runs,
	// exposed so a future web config editor can apply writes immediately
	// without a signal round-trip.
	Reload func(*config.Config) error
	// Store is the daemon's SampleStore for history queries. May be nil if
	// openConfiguredStore failed at startup (store-writes-disabled mode).
	Store SampleStore
	// Snapshot returns the latest merged Snapshot (backed by snapshotHub, an
	// atomic.Pointer[Snapshot] updated once per fast tick — see daemon.go).
	// Lock-free: safe to call from any goroutine, any number of times.
	Snapshot func() Snapshot
	// StateDir is the daemon's state directory (StateDir, or its test
	// override), for any state the web needs to keep alongside
	// baseline.json/alerts.json/etc. (e.g. users.json, sessions).
	StateDir string
	// AlertLogPath is the path to the append-only alert log
	// (alertlog.jsonl), for a future alerts page.
	AlertLogPath string
	// AlertStatePath is the path to alerts.json (ack state), for a future
	// alerts page's ack round-trip.
	AlertStatePath string
	// TestChannel sends a one-off test notification through the named
	// channel (config.Config.Channels), mirroring `serverwatch channel test
	// <name>` (channel.go's cmdChannelTest/sendTestNotification) — for the
	// web channels page's "Send test" button (issue #66). Built from
	// sendTestNotification directly in cmdDaemon (daemon.go), not adapted in
	// daemon_web.go: buildNotifier/sendTestNotification are untagged
	// (channel.go carries no `web` build tag), so this closure needs nothing
	// from internal/web to construct, unlike Snapshot/Store above.
	TestChannel func(name string) error
	// Enabled mirrors cfg.Web.Enabled (the web.enabled config key), read
	// once at daemon startup in cmdDaemon — see daemon.go.
	Enabled bool
	// Listen mirrors cfg.Web.Listen (the web.listen config key), a
	// "host:port" string validated by internal/config.Config.Set via
	// net.SplitHostPort. See Enabled above.
	Listen string
}

// snapshotHub holds the latest merged Snapshot the sampler loop has built,
// updated once per fast tick (see cmdDaemon in daemon.go). Reads
// (WebDeps.Snapshot, ultimately a future web dashboard/SSE handler) are
// lock-free via atomic.Pointer; the sampler loop is the sole writer.
var snapshotHub atomic.Pointer[Snapshot]

// latestSnapshot returns the most recent Snapshot stored in snapshotHub, or
// the zero Snapshot if the sampler loop hasn't published one yet (e.g. web
// code calling this before cmdDaemon's loop has run its first tick).
func latestSnapshot() Snapshot {
	if s := snapshotHub.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}
