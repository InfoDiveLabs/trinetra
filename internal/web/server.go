//go:build web

package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"serverwatch/internal/config"
)

// webauthnStub proves this file — and therefore the whole `-tags web` build
// graph — actually reaches into github.com/go-webauthn/webauthn, so
// TestDefaultBuildIsStdlibOnly (internal/serverwatch/buildtag_test.go)
// exercises a real third-party dependency rather than an empty stand-in.
// Task 4 (issue #60) replaces this with real registration/login ceremonies
// backed by a *webauthn.WebAuthn built from web.rp_id/web.origin.
var webauthnStub *webauthn.WebAuthn

// Deps is what the web server needs from the running daemon, expressed
// without importing internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go): Store and Snapshot are left opaque
// (`any`) here on purpose, since this task's routes (GET /assets/, GET /)
// don't yet consume either — a later task (dashboard/SSE, history) will
// define the minimal web-local interfaces those need, and
// internal/serverwatch/daemon_web.go will adapt serverwatch's concrete
// SampleStore/Snapshot into them at the call site, same as it already does
// for these two fields.
type Deps struct {
	// Cfg returns the current config (race-safe against the daemon's reload).
	Cfg func() *config.Config
	// Reload validates, persists, and applies a new config in-process.
	Reload func(*config.Config) error
	// Store is the daemon's sample store for history queries; opaque here
	// (see the type doc above), may be nil.
	Store any
	// Snapshot returns the latest merged snapshot; opaque here (see the type
	// doc above).
	Snapshot func() any
	// StateDir is the daemon's state directory.
	StateDir string
	// AlertLogPath is the path to the append-only alert log.
	AlertLogPath string
	// AlertStatePath is the path to the alert ack-state file.
	AlertStatePath string
	// Enabled mirrors cfg.Web.Enabled (the web.enabled config key, issue
	// #58), read once at daemon startup — see daemon.go's cmdDaemon.
	Enabled bool
	// Listen mirrors cfg.Web.Listen (the web.listen config key, issue #58),
	// a "host:port" string net.SplitHostPort-validated by
	// internal/config.Config.Set.
	Listen string
}

// Start is the web server's entry point: given Deps, it binds an
// http.Server on Deps.Listen when Deps.Enabled and returns a stop func that
// gracefully shuts it down. If Deps.Enabled is false, Start binds nothing
// and returns a no-op stop and a nil error — the daemon always calls
// maybeStartWeb/Start unconditionally (see internal/serverwatch/web_deps.go),
// so "disabled" has to be a valid, harmless outcome here rather than an
// error.
//
// issue #59 adds serving modes (proxy/autocert/manual) + rpID/origin
// validation, at which point a validation failure would also legitimately
// return a non-nil error even when Enabled is true.
func Start(d Deps) (stop func(), err error) {
	if !d.Enabled {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", d.Listen)
	if err != nil {
		return nil, fmt.Errorf("web: listen %s: %w", d.Listen, err)
	}
	srv := &http.Server{Handler: newHandler(d)}
	go srv.Serve(ln) //nolint:errcheck // Shutdown below always yields http.ErrServerClosed; nothing else to log yet.
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}
