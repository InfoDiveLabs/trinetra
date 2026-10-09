package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
)

// nonceCtxKey is the unexported context key securityHeaders stores the per-request CSP
// nonce under, for renderPageStatus (templates.go) to read back into PageData.Nonce.
type nonceCtxKey struct{}

// newNonce returns a fresh, unguessable per-request value for the CSP script-src
// 'nonce-...' directive: 16 random bytes.
func newNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("web: generate CSP nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// securityHeaders wraps next with the response headers the design doc's security checklist
// calls for on every request:
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := newNonce()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		h := w.Header()
		// script-src is STRICT: 'self' plus this request's nonce only, no unsafe-inline -- that's
		// where the real XSS risk is, and the single inline handler.
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

// nonceFromContext returns the CSP nonce securityHeaders generated for this request, or ""
// if the request didn't pass through securityHeaders.
func nonceFromContext(r *http.Request) string {
	v, _ := r.Context().Value(nonceCtxKey{}).(string)
	return v
}
