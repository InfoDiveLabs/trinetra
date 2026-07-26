//go:build web

package serverwatch

import "serverwatch/internal/web"

// maybeStartWeb is the `-tags web` half of the seam: it is the ONLY file in
// this package allowed to import internal/web (see web_deps.go's design
// note). It adapts serverwatch's native WebDeps into web.Deps — a
// serverwatch-free type — and delegates to web.Start.
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
	stop, _ := web.Start(wd)
	return stop
}
