//go:build !web

package serverwatch

// maybeStartWeb is the default (no `web` build tag) half of the seam: it
// never references internal/web, so the default `serverwatch` build's
// module graph never sees go-webauthn/x-crypto/etc. cmdDaemon always calls
// this unconditionally; only the linked build variant decides whether
// anything actually starts. See web_deps.go for the full design note.
func maybeStartWeb(WebDeps) func() {
	return func() {}
}
