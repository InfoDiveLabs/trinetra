//go:build web

package serverwatch

import (
	"fmt"

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
	wd := web.Deps{
		Cfg:            d.Cfg,
		Reload:         d.Reload,
		Store:          d.Store,
		Snapshot:       func() any { return d.Snapshot() },
		StateDir:       d.StateDir,
		AlertLogPath:   d.AlertLogPath,
		AlertStatePath: d.AlertStatePath,
		Enabled:        d.Enabled,
		Listen:         d.Listen,
	}
	stop, err := web.Start(wd)
	if err != nil {
		fmt.Fprintln(stderr, "web: failed to start, web UI disabled:", err)
		return func() {}
	}
	return stop
}
