// Package web implements the optional, embedded HTTP UI for server-monitor:
// passkey (WebAuthn) auth, RBAC, a live dashboard, history graphs, a web
// config editor, and an admin-curated public view. It is compiled ONLY into
// the `serverwatch-web` binary (`go build -tags web`); the default
// `serverwatch` binary never imports this package, which is what keeps
// third-party dependencies (github.com/go-webauthn/webauthn,
// golang.org/x/crypto/acme/autocert, etc.) out of the stdlib-only core.
//
// This package deliberately does NOT import serverwatch/internal/serverwatch
// — doing so would create an import cycle once serverwatch's build-tagged
// daemon_web.go imports this package to call Start. Instead, Start takes a
// web-local Deps (this file's sibling, server.go), and
// internal/serverwatch/daemon_web.go adapts serverwatch's own WebDeps into a
// Deps at the call site. See internal/serverwatch/web_deps.go for the full
// design note.
package web
