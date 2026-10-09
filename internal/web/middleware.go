package web

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// sessionCtxKey is the unexported context key sessionMiddleware stores the current
// request's *Session under.
type sessionCtxKey struct{}

// sessionMiddleware reads the sw_session cookie (if any) and, when it names a live session
// (SessionStore.Get), stashes it in the request context.
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

// sessionFromContext returns the *Session sessionMiddleware stashed for this request, or
// (nil, false) if there wasn't one.
func sessionFromContext(r *http.Request) (*Session, bool) {
	sess, ok := r.Context().Value(sessionCtxKey{}).(*Session)
	return sess, ok && sess != nil
}

// userCtxKey is the unexported context key userMiddleware stores the current request's
// *User under.
type userCtxKey struct{}

// userMiddleware resolves sessionFromContext's Session into the *User it names
// (store.Get(sess.UserID)) and stashes it in the request context.
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

// userFromContext returns the *User userMiddleware stashed for this request, or (nil,
// false) if the request is anonymous.
func userFromContext(r *http.Request) (*User, bool) {
	u, ok := r.Context().Value(userCtxKey{}).(*User)
	return u, ok && u != nil
}

// requireRole gates next behind the signed-in request's role satisfying min using the
// ranking viewer < responder < admin (roleRank): RoleViewer admits any valid role.
func requireRole(min Role, d Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := userFromContext(r)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		if roleRank(u.Role) < roleRank(min) {
			renderDenied(w, r, d)
			return
		}
		next(w, r)
	}
}

// csrfTokenFromRequest reads the caller-supplied CSRF token off r: the X-CSRF-Token header.
func csrfTokenFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-CSRF-Token"); t != "" {
		return t
	}
	return r.FormValue("csrf_token")
}

// requireCSRF wraps next so every unsafe-method request (POST/PUT/DELETE/ PATCH) must carry
// a CSRF token (csrfTokenFromRequest) matching the current session's Session.CSRF.
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
			// Constant-time compare so a token guess can't be narrowed by timing the response; the
			// empty-token guard also short-circuits before the compare.
			if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRF)) != 1 {
				http.Error(w, "forbidden: missing or invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// cookieSecure reports whether the sw_session cookie should carry the Secure attribute for
// r: true unless this server's own resolved view of the request's scheme.
func cookieSecure(r *http.Request) bool {
	origin := requestOriginFromContext(r)
	if origin == "" {
		// No withRequestOrigin middleware ran (e.g. a handler invoked
		// directly in a test) -- fall back to this request's own TLS state.
		return r.TLS != nil
	}
	return strings.HasPrefix(origin, "https://")
}

// setSessionCookie sets the sw_session cookie naming sess: HttpOnly, SameSite=Lax, Path=/,
// Secure per cookieSecure, MaxAge from ttl.
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
