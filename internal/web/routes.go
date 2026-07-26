//go:build web

package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"time"
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
	// The dashboard is viewer+ per the design doc's route table (only
	// /public is anonymous), so it's gated behind requireRole(RoleViewer,
	// ...): any signed-in account (viewer or admin) may see it; an anonymous
	// visitor is redirected to /login. The other viewer+ routes
	// (/history, /alerts, /monitoring) are gated the same way by the tasks
	// that register them.
	mux.HandleFunc("GET /{$}", requireRole(RoleViewer, dashboardHandler(d)))
	// /events (Task 8/#64): the SSE stream dashboard.html's live tiles/charts
	// subscribe to (assets/app.js's swBootSSE, sse.go). Viewer-gated exactly
	// like the dashboard itself — it carries the same live metrics, just
	// pushed instead of polled.
	mux.HandleFunc("GET /events", requireRole(RoleViewer, eventsHandler(d)))
	// /history + /api/series (Task 9/#65): time-range history graphs backed
	// by the daemon's SampleStore (Deps.Store, a SeriesStore — see
	// series_store.go). Viewer-gated exactly like the dashboard/events above:
	// history is read-only data, same role floor as the rest of "Monitor".
	mux.HandleFunc("GET /history", requireRole(RoleViewer, historyPageHandler(d)))
	mux.HandleFunc("GET /api/series", requireRole(RoleViewer, seriesAPIHandler(d)))
	mux.HandleFunc("GET /enroll", enrollPageHandler(d))
	mux.HandleFunc("POST /enroll/begin", enrollBeginHandler(d))
	mux.HandleFunc("POST /enroll/finish", enrollFinishHandler(d))
	mux.HandleFunc("GET /login", loginPageHandler(d))
	mux.HandleFunc("POST /login/begin", loginBeginHandler(d))
	mux.HandleFunc("POST /login/finish", loginFinishHandler(d))
	// /logout is a signed-in session's own mutation (not a pre-auth
	// ceremony endpoint like /enroll or /login), so it's CSRF-protected —
	// see requireCSRF's doc (middleware.go) for why those other POSTs
	// aren't.
	mux.Handle("POST /logout", requireCSRF(logoutHandler(d)))

	// Admin-only routes: the mockup app.js's ADMIN_PAGES list
	// (config.html/channels.html/users.html/public-settings.html), gated by
	// requireRole(RoleAdmin, ...) (middleware.go). Real content (config
	// editor, channel management, user management, public-view curation) is
	// later tasks' job — these are placeholders in exactly the same spirit
	// dashboardHandler was before the live-dashboard task, proving the
	// RBAC gate + shell wiring work before the pages have anything real to
	// show.
	mux.HandleFunc("GET /config", requireRole(RoleAdmin, adminPlaceholderHandler(d, "Configuration", "Thresholds, monitors, schedules, channels")))
	mux.HandleFunc("GET /channels", requireRole(RoleAdmin, adminPlaceholderHandler(d, "Channels", "Notification channel management")))
	mux.HandleFunc("GET /settings/public", requireRole(RoleAdmin, adminPlaceholderHandler(d, "Public view", "Curated public dashboard settings")))

	// /users (Task 7/#63): the real user-management page — list accounts,
	// issue/re-issue enrollment tokens, change roles, remove accounts, revoke
	// individual passkeys. GET is requireRole(RoleAdmin, ...) like the other
	// admin routes above; every mutation additionally needs requireCSRF
	// (usersMutation, handlers_users.go), since these change someone ELSE's
	// account rather than the caller's own session (unlike POST /logout,
	// which only needs the CSRF half).
	mux.HandleFunc("GET /users", requireRole(RoleAdmin, usersPageHandler(d)))
	mux.HandleFunc("POST /users/invite", usersMutation(usersInviteHandler(d)))
	mux.HandleFunc("POST /users/{id}/role", usersMutation(usersRoleHandler(d)))
	mux.HandleFunc("POST /users/{id}/remove", usersMutation(usersRemoveHandler(d)))
	mux.HandleFunc("POST /users/{id}/credentials/{credParam}/revoke", usersMutation(usersRevokeCredentialHandler(d)))

	// sessionMiddleware runs for every request so any handler/template can
	// read the current session (sessionFromContext) — including
	// requireCSRF above, which relies on it having already populated the
	// context by the time /logout's handler chain reaches it. userMiddleware
	// runs just inside it, resolving that session into the *User requireRole
	// and currentRole (templates.go) both read via userFromContext.
	sessions := newSessionStore(d.StateDir)
	users := newUserStore(d.StateDir)
	return securityHeaders(sessionMiddleware(sessions, userMiddleware(users, mux)))
}

// assetHandler wraps http.FileServer to force a deterministic Content-Type
// for the extensions the mockup shell needs (text/css, application/
// javascript) instead of relying on mime.TypeByExtension, which consults
// the host's /etc/mime.types on Unix and so isn't guaranteed to agree
// across machines. http.ServeContent (which FileServer calls internally)
// only fills in Content-Type when it isn't already set, so pre-setting it
// here wins.
//
// It also suppresses http.FileServer's built-in directory index: a request
// whose path ends in "/" (e.g. "/assets/" after StripPrefix leaves "/") is
// 404'd rather than served as an <a href> listing of every embedded asset.
// The web only ever links concrete files, so an index is pure information
// leakage.
func assetHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// After StripPrefix("/assets/"), "/assets/" arrives as "" and
		// "/assets/sub/" as "sub/"; both are directory requests that
		// http.FileServer would answer with an index — 404 them instead.
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
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

// adminPlaceholderHandler renders a bare "coming later" panel (templates/
// admin_placeholder.html) through the full app-shell layout for one of the
// admin-only routes (see newHandler's requireRole(RoleAdmin, ...) wiring):
// title/sub are threaded straight into PageData.Title/Sub the same way
// dashboardHandler does. Every caller has already passed requireRole by the
// time this runs, so it does no authorization of its own.
func adminPlaceholderHandler(d Deps, title, sub string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newPageData(r, title, sub, "ok")
		if err := renderPage(w, "admin_placeholder.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// enrollPageHandler renders the passkey-registration page (ported from
// ui-mockup/enroll.html — see templates/enroll.html) through the bare/
// centered layout (base_bare.html/BarePageData, templates.go): unlike the
// dashboard/app-shell pages, there's no signed-in session yet to fill a
// sidebar/topbar with. assets/app.js wires the page's form to
// /enroll/begin and /enroll/finish via navigator.credentials.create.
//
// Any ?token=... on this GET is threaded through to BarePageData.EnrollToken
// (templates.go) so enroll.html can stash it in a hidden field and app.js
// can echo it back as /enroll/begin's "token" field — this handler itself
// does not consume/validate the token (that's enrollBeginHandler's job, via
// resolveEnrollRole/tokenStore.Redeem); a page load must stay side-effect
// free (a token is single-use and shouldn't burn on a mere GET or refresh).
func enrollPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newBarePageData(r, "Set up passkey")
		if err := renderBarePage(w, "enroll.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// enrollBeginRequest is POST /enroll/begin's JSON body: the account name
// typed into the enroll page's #enrollName input, plus an optional
// enrollment token (enroll.html's hidden #enrollToken field, populated from
// this page's own ?token= query parameter — see enrollPageHandler).
type enrollBeginRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// enrollBeginHandler starts a WebAuthn registration ceremony (beginRegistration,
// auth_webauthn.go) for the posted name, creating a brand-new *User (not yet
// persisted — finishRegistration's store.Put is what actually writes it).
//
// SECURITY: this endpoint is UNAUTHENTICATED, so it must ONLY ever create a
// new account — it must never attach a credential to an existing one. An
// earlier version looked the name up with store.ByName and, on a match, ran
// the ceremony against the existing *User (with its existing role); that was
// a cross-account credential-injection / account-takeover bug (an anonymous
// caller could POST name="admin" and bind their own passkey to the admin
// account). So a name that already exists is rejected with 409 here.
// Adding a second passkey to an EXISTING account (multi-device) must
// instead go through an authenticated session (the account's own owner) or
// a future admin-managed flow — never this anonymous path.
//
// Role assignment (issue #62, resolved): resolveEnrollRole
// (enroll_tokens.go) decides how the new account proceeds — an admin-issued
// enrollment token's Role if one was posted (tokenStore.Redeem also
// enforces the token being unknown/expired/already-used, and burns the
// single-use token now), or a tokenless first-run BOOTSTRAP attempt when no
// account exists yet, or a flat refusal once any account already exists:
// unauthenticated open enrollment is only ever valid for that first
// account. For a bootstrap attempt the admin role is NOT assigned here —
// that decision is deferred to finish time (finishRegistration ->
// jsonUserStore.CreateFirstAdmin, under the write lock) so two concurrent
// tokenless enrollments can't both observe an empty store and both become
// admin (the bootstrap TOCTOU).
func enrollBeginHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req enrollBeginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			http.Error(w, "name is required", http.StatusBadRequest)
			return
		}

		wa, err := webAuthnConfig(d.Cfg(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		store := newUserStore(d.StateDir)
		if _, exists := store.ByName(name); exists {
			// Never attach to an existing account from this unauthenticated
			// endpoint — see the SECURITY note above. Checked BEFORE any
			// token is redeemed, so a name collision never burns an
			// otherwise-valid invite token.
			http.Error(w, "an account with that name already exists; adding a passkey to an existing account will require an admin invite", http.StatusConflict)
			return
		}

		role, bootstrap, err := resolveEnrollRole(newTokenStore(d.StateDir), store, strings.TrimSpace(req.Token))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		id, err := newUserID()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// For a bootstrap (tokenless first-run) enrollment, role is left empty
		// here on purpose: finishRegistration -> CreateFirstAdmin assigns
		// RoleAdmin atomically with the write, so the "first account becomes
		// admin" decision can't be duplicated by two racing enrollments. For a
		// token enrollment, role is already the token's final grant.
		u := &User{ID: id, Name: name, Role: role, Created: time.Now().Unix()}

		ceremonies := newCeremonyStore(d.StateDir)
		creation, err := beginRegistration(w, r, wa, u, bootstrap, ceremonies)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(creation); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// enrollFinishHandler completes the ceremony enrollBeginHandler started: it
// verifies the browser's attestation response (the request body) against
// the session enrollBeginHandler stashed (finishRegistration, keyed by the
// enrollSessionCookie it set) and, only on success, persists the new
// credential to <StateDir>/users.json.
func enrollFinishHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wa, err := webAuthnConfig(d.Cfg(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		store := newUserStore(d.StateDir)
		ceremonies := newCeremonyStore(d.StateDir)
		if err := finishRegistration(w, r, wa, store, ceremonies); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// loginPageHandler renders the passkey sign-in page (ported from
// ui-mockup/login.html — see templates/login.html) through the bare/
// centered layout, same as enrollPageHandler: there's no session yet to fill
// an app-shell sidebar/topbar with.
func loginPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newBarePageData(r, "Sign in")
		if err := renderBarePage(w, "login.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// loginBeginHandler starts a WebAuthn login (assertion) ceremony
// (beginLogin, auth_webauthn.go) using client-side discoverable
// credentials — the login page's single "Continue with passkey" button
// posts here with no body, no username.
func loginBeginHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wa, err := webAuthnConfig(d.Cfg(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ceremonies := newCeremonyStore(d.StateDir)
		assertion, err := beginLogin(w, r, wa, ceremonies)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(assertion); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// loginFinishHandler completes the ceremony loginBeginHandler started: it
// verifies the browser's assertion response (the request body) against the
// session loginBeginHandler stashed (finishLogin, keyed by the
// loginCeremonyCookie it set), resolves the signing-in account from the
// assertion's userHandle, rejects a cloned-authenticator signCount
// regression, and — only on success — sets the sw_session cookie.
func loginFinishHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wa, err := webAuthnConfig(d.Cfg(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		store := newUserStore(d.StateDir)
		ceremonies := newCeremonyStore(d.StateDir)
		sessions := newSessionStore(d.StateDir)
		if err := finishLogin(w, r, wa, store, ceremonies, sessions, sessionTTL(d.Cfg())); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// logoutHandler deletes the caller's signed-in session (if any — see
// requireCSRF's route wiring in newHandler, which already required a valid
// session/CSRF pair to reach here) and clears the sw_session cookie.
// Idempotent: a repeat call (or one with no session, which requireCSRF
// would already have rejected) just clears the cookie again.
func logoutHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessions := newSessionStore(d.StateDir)
		if sess, ok := sessionFromContext(r); ok {
			_ = sessions.Delete(sess.ID)
		}
		clearSessionCookie(w, r)
		w.WriteHeader(http.StatusNoContent)
	}
}
