// Package web implements the optional HTTP UI for server-monitor: passkey
// (WebAuthn) auth, RBAC, a live dashboard, history graphs, a web config
// editor, and an admin-curated public view. This package is compiled into
// the `serverwatch-web` binary (no build tag); the default `serverwatch`
// binary never imports this package, which is what keeps third-party
// dependencies (github.com/go-webauthn/webauthn,
// golang.org/x/crypto/acme/autocert, etc.) out of the stdlib-only core.
//
// This package deliberately does NOT import serverwatch/internal/serverwatch:
// the daemon never imports internal/web either, so the two stay on opposite
// sides of the process boundary. Instead, Start takes a web-local Deps
// (this file's sibling, server.go), and cmd/serverwatch-web adapts a
// *control.Client, dialed over the daemon's control socket, into a Deps at
// the call site. See internal/serverwatch/web_supervisor.go for how the
// core daemon supervises this binary.
package web
