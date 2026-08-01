package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
)

// nonceCtxKey is the unexported context key securityHeaders stores the
// per-request CSP nonce under, for renderPage (routes.go/templates.go) to
// read back into PageData.Nonce.
type nonceCtxKey struct{}

// newNonce returns a fresh, unguessable per-request value for the CSP
// script-src 'nonce-...' directive: 16 random bytes (128 bits, well above
// the 8-byte minimum the CSP spec recommends for nonces), URL-safe
// base64-encoded so it drops cleanly into both an HTTP header value and an
// HTML attribute without escaping.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("web: generate CSP nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// securityHeaders wraps next with the response headers the design doc's
// security checklist calls for on every request:
//
//   - Content-Security-Policy scoped to 'self' (this server's own embedded
//     assets -- see assets.go/routes.go, no CDN) plus a fresh per-request
//     nonce that authorizes exactly one script: the htmx boot tag
//     (templates/base.html), and no unsafe-inline escape hatch.
//   - X-Content-Type-Options: nosniff, so a browser never MIME-sniffs a
//     response away from the Content-Type routes.go set.
//   - Referrer-Policy, so cross-origin navigations (e.g. an admin clicking
//     an external link from the dashboard) don't leak this host's internal
//     paths in the Referer header.
//   - Strict-Transport-Security, but ONLY when the current request actually
//     arrived over TLS (r.TLS != nil) -- true exactly in the autocert/manual
//     modes, where this process itself terminates TLS; in proxy mode this
//     process only ever sees plain HTTP even when a front proxy terminates
//     TLS upstream, so sending HSTS here would be a promise about a
//     connection this process never made.
//
// The nonce is also stashed in the request context (nonceFromContext) so a
// page handler can thread it into PageData.Nonce for the template.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := newNonce()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		h := w.Header()
		// script-src is STRICT: 'self' plus this request's nonce only, no
		// unsafe-inline -- that's where the real XSS risk is, and the single
		// inline handler (the theme button) was moved into app.js so nothing
		// needs it. style-src, however, allows 'unsafe-inline': the ported
		// mockup (our required visual base) uses inline style="…" attributes
		// pervasively across every page; inline styles are low XSS risk and
		// rewriting them all into classes is out of scope ("mockup as base").
		h.Set("Content-Security-Policy", fmt.Sprintf(
			"default-src 'self'; script-src 'self' 'nonce-%s'; style-src 'self' 'unsafe-inline'; img-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'",
			nonce,
		))
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}

		ctx := context.WithValue(r.Context(), nonceCtxKey{}, nonce)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// nonceFromContext returns the CSP nonce securityHeaders generated for this
// request, or "" if the request didn't pass through securityHeaders (e.g. a
// handler invoked directly in a test).
func nonceFromContext(r *http.Request) string {
	v, _ := r.Context().Value(nonceCtxKey{}).(string)
	return v
}
