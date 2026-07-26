//go:build web

package web

import (
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
// (`any`) here on purpose, since this task's Start is a stub that doesn't
// yet consume either — a later task (dashboard/SSE, history) will define the
// minimal web-local interfaces those need, and
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
	// Enabled mirrors cfg.Web.Enabled (config keys land in issue #58).
	Enabled bool
	// Listen mirrors cfg.Web.Listen (config keys land in issue #58).
	Listen string
}

// Start is the web server's entry point: given Deps, it will bind an
// http.Server on Deps.Listen when Deps.Enabled and return a stop func that
// gracefully shuts it down. For now (issue #57, module wiring only) it is a
// stub: no listener is bound yet, and it always returns a no-op stop and a
// nil error. issue #58 implements the actual ServeMux/embedded-assets/base
// layout; issue #59 adds serving modes + rpID/origin validation, at which
// point a validation failure here would legitimately return a non-nil error.
func Start(d Deps) (stop func(), err error) {
	return func() {}, nil
}
