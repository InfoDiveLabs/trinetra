//go:build web

package web

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// sessionCtxKey is the unexported context key sessionMiddleware stores the
// current request's *Session under (only present when the sw_session
// cookie names a live, non-expired session), for requireCSRF/templates to
// read back via sessionFromContext.
type sessionCtxKey struct{}

// sessionMiddleware reads the sw_session cookie (if any) and, when it names
// a live session (SessionStore.Get), stashes it in the request context.
// It always calls next regardless of whether a session was found —
// requiring one is a per-route concern (requireCSRF for CSRF-protected
// mutations; requireRole, below, for auth-required pages), not this
// middleware's job.
func sessionMiddleware(store SessionStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookieName); err == nil {
			if sess, ok := store.Get(c.Value); ok {
				r = r.WithContext(context.WithValue(r.Context(), sessionCtxKey{}, sess))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// sessionFromContext returns the *Session sessionMiddleware stashed for this
// request, or (nil, false) if there wasn't one (no cookie, unknown ID, or
// expired — SessionStore.Get treats an expired record as absent, which is
// exactly what should make a request look unauthenticated here too).
func sessionFromContext(r *http.Request) (*Session, bool) {
	sess, ok := r.Context().Value(sessionCtxKey{}).(*Session)
	return sess, ok && sess != nil
}

// userCtxKey is the unexported context key userMiddleware stores the
// current request's *User under (only present when sessionMiddleware found
// a live session AND that session's UserID resolves to a real account),
// for requireRole/currentRole (templates.go) to read back via
// userFromContext.
type userCtxKey struct{}

// userMiddleware resolves sessionFromContext's Session into the *User it
// names (store.Get(sess.UserID)) and stashes it in the request context, the
// "user" step of requireRole's doc'd "session→user→role gate". It runs
// after sessionMiddleware (routes.go's newHandler wiring) and, like it,
// always calls next regardless of whether a user was resolved — requiring
// one is requireRole's job, not this middleware's.
//
// A session with an empty UserID (an in-flight WebAuthn ceremony
// placeholder — see Session.UserID's doc) never resolves to a user here,
// but in practice one never reaches this middleware anyway: ceremony
// placeholders live under the separate sw_enroll/sw_login cookies, not
// sw_session, so sessionFromContext never surfaces one to begin with.
func userMiddleware(store UserStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess, ok := sessionFromContext(r); ok && sess.UserID != "" {
			if u, ok := store.Get(sess.UserID); ok {
				r = r.WithContext(context.WithValue(r.Context(), userCtxKey{}, u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// userFromContext returns the *User userMiddleware stashed for this
// request, or (nil, false) if the request is anonymous (no session, or a
// session whose UserID no longer resolves to an account — e.g. a deleted
// user with a still-live session record).
func userFromContext(r *http.Request) (*User, bool) {
	u, ok := r.Context().Value(userCtxKey{}).(*User)
	return u, ok && u != nil
}

// requireRole gates next behind the signed-in request's role satisfying
// min: RoleViewer admits any authenticated user (viewer or admin);
// RoleAdmin admits only admins. This is the RBAC gate the design doc's
// "middleware maps session→user→role" line calls for — routes.go's
// newHandler wires it onto the admin-only routes (config/channels/users/
// public-settings, the mockup app.js's ADMIN_PAGES).
//
// No session/user at all (anonymous) redirects to /login regardless of
// min: an anonymous visitor isn't unauthorized, they just haven't signed in
// yet, so send them to do that. A signed-in user whose role falls short of
// min instead renders the mockup's "Admin only" denied panel with 403 —
// they ARE authenticated, just not authorized, so bouncing them to /login
// would accomplish nothing (their passkey already works fine).
//
// d is threaded through only so a 403 can render the denied panel with the
// same live nav (renderDenied -> newPageData -> navCountsFor, templates.go)
// every other page shows — it plays no role in the authorization decision
// itself.
func requireRole(min Role, d Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFromContext(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if min == RoleAdmin && u.Role != RoleAdmin {
			renderDenied(w, r, d)
			return
		}
		next(w, r)
	}
}

// csrfTokenFromRequest reads the caller-supplied CSRF token off r: the
// X-CSRF-Token header (what app.js sends alongside a fetch()'d mutation,
// reading it from base.html's csrf-token meta tag) or, failing that, a
// csrf_token form field (a plain HTML form POST without JS). r.FormValue
// only actually parses the body for a application/x-www-form-urlencoded or
// multipart/form-data Content-Type, so this is a no-op/empty-string read for
// the JSON POST bodies /enroll and /login use.
func csrfTokenFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-CSRF-Token"); t != "" {
		return t
	}
	return r.FormValue("csrf_token")
}

// requireCSRF wraps next so every unsafe-method request (POST/PUT/DELETE/
// PATCH) must carry a CSRF token (csrfTokenFromRequest) matching the
// current session's Session.CSRF — a missing session (no cookie, unknown,
// or expired), a missing token, or a mismatched token all reject with 403.
// Safe methods (GET/HEAD/OPTIONS/...) pass straight through.
//
// This is deliberately NOT wired onto /enroll/begin, /enroll/finish,
// /login/begin, or /login/finish: those are unauthenticated ceremony
// endpoints with no session yet to hold a CSRF token in the first place —
// WebAuthn's own origin/challenge binding (webAuthnConfig, beginRegistration/
// finishRegistration/beginLogin/finishLogin) is what protects them instead.
// requireCSRF guards mutations made BY an already signed-in session (e.g.
// POST /logout, and later tasks' config/user-management writes).
func requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			sess, ok := sessionFromContext(r)
			if !ok {
				http.Error(w, "forbidden: no session", http.StatusForbidden)
				return
			}
			token := csrfTokenFromRequest(r)
			// Constant-time compare so a token guess can't be narrowed by
			// timing the response; the empty-token guard also short-circuits
			// before the compare (a missing token is never valid).
			if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// cookieSecure reports whether the sw_session cookie should carry the
// Secure attribute for r: true unless this server's own resolved view of
// the request's scheme (requestOriginFromContext — see serving.go's
// withRequestOrigin/isLoopbackAddr, which only trusts a request's
// X-Forwarded-Proto when the listener is loopback-bound, i.e. a local
// reverse proxy is plausibly terminating TLS in front of us) is plain http.
// That happens in exactly two cases: a genuinely insecure connection, or
// the loopback-bound dev exception (no proxy, no TLS, testing directly
// against http://localhost:...) — in both, a Secure cookie would simply
// never be sent back by the browser, breaking the session outright.
func cookieSecure(r *http.Request) bool {
	origin := requestOriginFromContext(r)
	if origin == "" {
		// No withRequestOrigin middleware ran (e.g. a handler invoked
		// directly in a test) — fall back to this request's own TLS state.
		return r.TLS != nil
	}
	return strings.HasPrefix(origin, "https://")
}

// setSessionCookie sets the sw_session cookie naming sess: HttpOnly,
// SameSite=Lax, Path=/, Secure per cookieSecure, MaxAge from ttl — the shape
// this task's cookie requirement specifies.
func setSessionCookie(w http.ResponseWriter, r *http.Request, sess *Session, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	})
}

// clearSessionCookie expires the sw_session cookie immediately (logout).
func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
