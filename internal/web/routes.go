//go:build web

package web

import (
	"io/fs"
	"net/http"
	"strings"
)

// newHandler builds the full ServeMux Start binds an http.Server around.
// Kept separate from Start so it's testable via httptest.NewRecorder
// without binding a real port (see server_test.go).
func newHandler(d Deps) http.Handler {
	assetsSub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// assetsFS is a compile-time //go:embed of a directory that exists
		// in this package (assets.go); fs.Sub can only fail here if that
		// invariant is broken, which is a build-time bug, not a runtime one.
		panic("internal/web: assets embed missing \"assets\" dir: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assetHandler(assetsSub)))
	mux.HandleFunc("GET /{$}", dashboardHandler(d))
	return mux
}

// assetHandler wraps http.FileServer to force a deterministic Content-Type
// for the extensions the mockup shell needs (text/css, application/
// javascript) instead of relying on mime.TypeByExtension, which consults
// the host's /etc/mime.types on Unix and so isn't guaranteed to agree
// across machines. http.ServeContent (which FileServer calls internally)
// only fills in Content-Type when it isn't already set, so pre-setting it
// here wins.
func assetHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := contentTypeByExt(r.URL.Path); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		fileServer.ServeHTTP(w, r)
	})
}

// contentTypeByExt returns the Content-Type this package's vendored/mockup
// assets need, or "" to let http.FileServer's default sniffing/mime lookup
// decide (harmless for extensions this handler doesn't special-case).
func contentTypeByExt(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	}
	return ""
}

// dashboardHandler renders the dashboard placeholder inside the base
// layout. A later task (dashboard/SSE) replaces the placeholder content
// with the live summary counts/tiles/alerts the mockup's dashboard.html
// shows; this task only needs the shell + routing to work.
func dashboardHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newPageData(r, "Dashboard", "Overview", "ok")
		if err := renderPage(w, "dashboard.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
