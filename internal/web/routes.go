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
	// / is the true landing route (public-rework task): an authenticated
	// visitor (viewer or admin) sees the dashboard, exactly like the old
	// requireRole(RoleViewer, ...) wiring; an anonymous one sees the curated
	// public status page when cfg.Public.Enabled, else is redirected to
	// /login -- see rootHandler's doc (handlers_public.go) for the full
	// branch and its SECURITY note (an anonymous request must never reach
	// the dashboard). The other viewer+ routes (/history, /alerts,
	// /monitoring) stay gated by requireRole(RoleViewer, ...) as before --
	// only / itself has a third, anonymous-but-not-login-redirected outcome.
	mux.HandleFunc("GET /{$}", rootHandler(d))
	// /events (Task 8/#64): the SSE stream dashboard.html's live tiles/charts
	// subscribe to (assets/app.js's swBootSSE, sse.go). Viewer-gated exactly
	// like the dashboard itself -- it carries the same live metrics, just
	// pushed instead of polled.
	mux.HandleFunc("GET /events", requireRole(RoleViewer, d, eventsHandler(d)))
	// /history + /api/series (Task 9/#65): time-range history graphs backed
	// by the daemon's SampleStore, reached through Deps.API.Series (core.API,
	// see server.go's Deps.API doc). Viewer-gated exactly like the
	// dashboard/events above: history is read-only data, same role floor as
	// the rest of "Monitor".
	mux.HandleFunc("GET /history", requireRole(RoleViewer, d, historyPageHandler(d)))
	mux.HandleFunc("GET /api/series", requireRole(RoleViewer, d, seriesAPIHandler(d)))
	mux.HandleFunc("GET /api/downtime", requireRole(RoleViewer, d, downtimeAPIHandler(d)))
	// /monitoring: the detailed per-entity view (containers/systemd units/
	// processes/filesystems), ported from ui-mockup/monitoring.html. Same
	// viewer+ floor as the rest of "Monitor" -- see monitoringHandler
	// (handlers_monitoring.go) and web.MonitoringView/
	// internal/trinetra/coreapi_inproc.go's buildMonitoringView adapter for
	// where its data comes from.
	mux.HandleFunc("GET /monitoring", requireRole(RoleViewer, d, monitoringHandler(d)))
	// /host (#100): the static host hardware/OS inventory, read over the
	// control socket via Deps.API.HostInfo. Viewer-gated like the other Monitor
	// pages.
	mux.HandleFunc("GET /host", requireRole(RoleViewer, d, hostPageHandler(d)))
	// /api/container/logs (#115): a docker-logs snapshot for the dashboard
	// drawer's "View logs" action. Admin-gated -- logs can carry secrets.
	mux.HandleFunc("GET /api/container/logs", requireRole(RoleAdmin, d, containerLogsHandler(d)))
	// /fleet + /fleet/table + /api/fleet/nodes (Task 5/fleet-web-a): the
	// fleet overview -- health strip, filterable/sortable node table, and
	// its htmx poll fragment/JSON API. Viewer-gated exactly like the rest
	// of "Monitor" (RBAC doesn't distinguish master from solo/child here);
	// each handler itself 404s unless fleetRole(d)=="master"
	// (handlers_fleet.go), so a solo/child daemon's viewer sees a plain
	// not-found rather than an empty fleet page. /fleet is also on
	// node_scope.go's masterLocalPrefixes, so /n/{node}/fleet is already a
	// 404 via withNodeRouter -- these three routes only ever render the
	// master's own view.
	mux.HandleFunc("GET /fleet", requireRole(RoleViewer, d, fleetOverviewHandler(d)))
	mux.HandleFunc("GET /fleet/table", requireRole(RoleViewer, d, fleetTableHandler(d)))
	mux.HandleFunc("GET /api/fleet/nodes", requireRole(RoleViewer, d, fleetNodesAPIHandler(d)))
	// /fleet/compare (Task C1b, plan C): the fleet-wide metric compare view
	// over FleetAPI.FleetSeries -- ?nodes=a,b,c (the table's "Compare"
	// action below) or ?tag=web selects the nodes, one uPlot overlay per
	// request. Viewer-gated and master-only exactly like the read-only
	// /fleet routes above; already on node_scope.go's masterLocalPrefixes
	// via its "/fleet" entry.
	mux.HandleFunc("GET /fleet/compare", requireRole(RoleViewer, d, fleetCompareHandler(d)))
	// /fleet/incidents + /fleet/incidents/{id} (+ /table poll fragment, +
	// ack/silence mutations) -- task C2, plan C: the fleet incidents web UI
	// over FleetAPI's Incidents/Incident/AckIncident/Explain/CreateSilence.
	// GETs are viewer-gated and master-only (fleetGateHTML) exactly like the
	// rest of "Monitor"; ack/silence are admin+CSRF (fleetAdminMutation),
	// same as /fleet/admin's mutations below. Already covered by
	// node_scope.go's masterLocalPrefixes "/fleet" entry (prefix-matches
	// every /fleet/... path), so /n/{node}/fleet/incidents... is already a
	// 404 via withNodeRouter.
	mux.HandleFunc("GET /fleet/incidents", requireRole(RoleViewer, d, fleetIncidentsHandler(d)))
	mux.HandleFunc("GET /fleet/incidents/table", requireRole(RoleViewer, d, fleetIncidentsTableHandler(d)))
	mux.HandleFunc("GET /fleet/incidents/{id}", requireRole(RoleViewer, d, fleetIncidentHandler(d)))
	mux.HandleFunc("POST /fleet/incidents/{id}/ack", fleetAdminMutation(d, fleetIncidentAckHandler(d)))
	mux.HandleFunc("POST /fleet/incidents/{id}/silence", fleetAdminMutation(d, fleetIncidentSilenceHandler(d)))
	// /fleet/alerting (+ /test, + /fleet/rules/state) -- task C3, plan C:
	// the routing/escalation config editor (routes/policies/rules, "edit as
	// JSON"), the route tester, and the rule-state fragment, over
	// FleetAPI's Alerting/SetAlerting/RouteTest/RuleStates. GET is
	// viewer-gated (read-only for a viewer -- the page renders no <form>
	// for anything but the tester at that role) and master-only
	// (fleetGateHTML, handlers_fleet_alerting.go); the save POST is
	// admin+CSRF (fleetAdminMutation), exactly like /fleet/admin's
	// mutations. The route tester POST stays viewer-gated (it only ever
	// dry-runs RouteTest, it never mutates the saved config) but still
	// requires CSRF (fix round 1 IMPORTANT 1) -- the same precedent
	// POST /channels/{name}/test sets for a non-mutating-but-still-
	// session-triggered action: any signed-in POST that could be forged
	// cross-site gets a CSRF check regardless of whether it happens to
	// write anything. Already covered by node_scope.go's
	// masterLocalPrefixes "/fleet" entry.
	mux.HandleFunc("GET /fleet/alerting", requireRole(RoleViewer, d, fleetAlertingPageHandler(d)))
	mux.HandleFunc("POST /fleet/alerting", limitBody(alertingMaxBodyBytes, fleetAdminMutation(d, fleetAlertingSaveHandler(d))))
	mux.HandleFunc("POST /fleet/alerting/test", requireRole(RoleViewer, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(fleetAlertingTestHandler(d)).ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /fleet/rules/state", requireRole(RoleViewer, d, fleetRulesStateHandler(d)))
	// /fleet/admin + /fleet/tokens*/ + /fleet/nodes/* (Task 7, fleet-web-a):
	// node management (rename/tags/revoke/remove) and join-token
	// issuance/revocation, admin+CSRF-gated like /users/* below -- see
	// fleetAdminMutation's doc. Each handler additionally 404s unless
	// fleetRole(d)=="master" (fleetGateHTML, handlers_fleet_admin.go),
	// exactly like the read-only /fleet routes above. "/fleet/admin" is
	// already on node_scope.go's masterLocalPrefixes (its "/fleet" entry
	// prefix-matches every /fleet/... path), so /n/{node}/fleet/admin is
	// already a 404 via withNodeRouter.
	mux.HandleFunc("GET /fleet/admin", requireRole(RoleAdmin, d, fleetAdminPageHandler(d)))
	mux.HandleFunc("POST /fleet/tokens", fleetAdminMutation(d, fleetTokenCreateHandler(d)))
	mux.HandleFunc("POST /fleet/tokens/{id}/delete", fleetAdminMutation(d, fleetTokenDeleteHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/rename", fleetAdminMutation(d, fleetNodeRenameHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/tags", fleetAdminMutation(d, fleetNodeTagsHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/revoke", fleetAdminMutation(d, fleetNodeRevokeHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/remove", fleetAdminMutation(d, fleetNodeRemoveHandler(d)))
	// beginLimiter caps the unauthenticated ceremony-begin rate per client so an
	// anonymous caller can't hammer the shared ceremonies.json lock (#95). Both
	// begins share ONE limiter since they contend the same lock. finish is not
	// limited: it needs a valid in-flight ceremony (cookie + challenge) a begin
	// already gated.
	beginLimiter := newRateLimiter(beginRateMax, beginRateWindow)
	mux.HandleFunc("GET /enroll", enrollPageHandler(d))
	mux.HandleFunc("POST /enroll/begin", rateLimitBegin(beginLimiter, enrollBeginHandler(d)))
	mux.HandleFunc("POST /enroll/finish", enrollFinishHandler(d))
	mux.HandleFunc("GET /login", loginPageHandler(d))
	mux.HandleFunc("POST /login/begin", rateLimitBegin(beginLimiter, loginBeginHandler(d)))
	mux.HandleFunc("POST /login/finish", loginFinishHandler(d))
	// /logout is a signed-in session's own mutation (not a pre-auth
	// ceremony endpoint like /enroll or /login), so it's CSRF-protected --
	// see requireCSRF's doc (middleware.go) for why those other POSTs
	// aren't.
	mux.Handle("POST /logout", requireCSRF(logoutHandler(d)))

	// Admin-only routes: the mockup app.js's ADMIN_PAGES list
	// (config.html/channels.html/users.html/public-settings.html), gated by
	// requireRole(RoleAdmin, ...) (middleware.go). Real content (config
	// editor, channel management, user management, public-view curation) is
	// later tasks' job -- these are placeholders in exactly the same spirit
	// dashboardHandler was before the live-dashboard task, proving the
	// RBAC gate + shell wiring work before the pages have anything real to
	// show.
	// /config (Task 10/#66): the real config editor -- thresholds, monitors
	// (enable/disable + per-target threshold), schedules, quiet hours. GET is
	// requireRole(RoleAdmin, ...) like the other admin routes; POST additionally
	// needs requireCSRF (configMutation, handlers_config.go), since it's a
	// config-wide mutation exactly like the /users/* mutations below.
	mux.HandleFunc("GET /config", requireRole(RoleAdmin, d, configPageHandler(d)))
	mux.HandleFunc("POST /config", configMutation(d, configSaveHandler(d)))
	// /channels (Task 10/#66): CRUD over config.Channels, ported from
	// ui-mockup/channels.html's table + add/edit modal. GET is
	// requireRole(RoleAdmin, ...) like /config; every mutation additionally
	// needs requireCSRF (channelsMutation, handlers_channels.go).
	mux.HandleFunc("GET /channels", requireRole(RoleAdmin, d, channelsPageHandler(d)))
	mux.HandleFunc("POST /channels", channelsMutation(d, channelsAddHandler(d)))
	mux.HandleFunc("POST /channels/{name}/update", channelsMutation(d, channelsUpdateHandler(d)))
	mux.HandleFunc("POST /channels/{name}/remove", channelsMutation(d, channelsRemoveHandler(d)))
	mux.HandleFunc("POST /channels/{name}/test", channelsMutation(d, channelsTestHandler(d)))
	// /settings/public + /public + /public/events (Task 11/#67, extended by
	// the public-rework task): the admin-curated exposure picker (GET/POST
	// /settings/public, admin-only + CSRF on the mutation --
	// publicSettingsMutation, handlers_public.go); GET /public itself is now
	// just a redirect to / (publicRouteRedirectHandler,
	// handlers_public.go -- the anonymous page moved to / itself, see
	// rootHandler); and GET /public/events is the anonymous page's live SSE
	// counterpart (publicEventsHandler, sse.go) -- deliberately NOT gated by
	// requireRole/requireCSRF, same as / itself: it enforces its own
	// "disabled -> 404" + server-side panel allowlist instead (see that
	// handler's SECURITY doc). Never reuses /events (the viewer-gated
	// stream) -- a shared endpoint would mean either leaking the full
	// DashboardView anonymously or threading an allowlist filter through a
	// handler that also serves authenticated viewers, both worse than a
	// second, narrowly-scoped handler.
	mux.HandleFunc("GET /settings/public", requireRole(RoleAdmin, d, publicSettingsPageHandler(d)))
	mux.HandleFunc("POST /settings/public", publicSettingsMutation(d, publicSettingsSaveHandler(d)))
	mux.HandleFunc("GET /public", publicRouteRedirectHandler)
	mux.HandleFunc("GET /public/events", publicEventsHandler(d))

	// /alerts (Task 10/#66): alert history + active alerts, both read over
	// the control socket (Deps.API.AlertHistory/ActiveAlerts), viewer+ per
	// the design doc -- this
	// resolves the earlier placeholder note that /alerts must be
	// viewer-gated, not admin-only. Ack, however, is admin-only + CSRF: it
	// mutates shared alert state everyone else's view depends on.
	mux.HandleFunc("GET /alerts", requireRole(RoleViewer, d, alertsPageHandler(d)))
	mux.HandleFunc("POST /alerts/{key}/ack", requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(alertsAckHandler(d)).ServeHTTP(w, r)
	}))

	// /users (Task 7/#63): the real user-management page -- list accounts,
	// issue/re-issue enrollment tokens, change roles, remove accounts, revoke
	// individual passkeys. GET is requireRole(RoleAdmin, ...) like the other
	// admin routes above; every mutation additionally needs requireCSRF
	// (usersMutation, handlers_users.go), since these change someone ELSE's
	// account rather than the caller's own session (unlike POST /logout,
	// which only needs the CSRF half).
	mux.HandleFunc("GET /users", requireRole(RoleAdmin, d, usersPageHandler(d)))
	mux.HandleFunc("POST /users/invite", usersMutation(d, usersInviteHandler(d)))
	mux.HandleFunc("POST /users/{id}/role", usersMutation(d, usersRoleHandler(d)))
	mux.HandleFunc("POST /users/{id}/remove", usersMutation(d, usersRemoveHandler(d)))
	mux.HandleFunc("POST /users/{id}/credentials/{credParam}/revoke", usersMutation(d, usersRevokeCredentialHandler(d)))

	// sessionMiddleware runs for every request so any handler/template can
	// read the current session (sessionFromContext) -- including
	// requireCSRF above, which relies on it having already populated the
	// context by the time /logout's handler chain reaches it. userMiddleware
	// runs just inside it, resolving that session into the *User requireRole
	// and currentRole (templates.go) both read via userFromContext.
	sessions := newSessionStore(d.StateDir)
	users := newUserStore(d.StateDir)
	// gzipMiddleware (Task 7, slow-request-resilience) is the outermost
	// wrap: it compresses large responses (history/series JSON) for
	// congested uplinks, gated on Accept-Encoding: gzip and a 1KB minimum,
	// and excludes /events + /public/events (SSE streams -- see its doc in
	// compress.go for why buffering those would break live push).
	//
	// withNodeRouter (node_scope.go, fleet-web-a task 1) sits just inside
	// userMiddleware, not outside it: a /n/{node}/... request must already
	// carry the same resolved session/user every other request does before
	// requireRole/requireCSRF on the re-dispatched (prefix-stripped)
	// request evaluate it -- node scoping never bypasses auth.
	//
	// withFleetMemo (fleet_memo.go, fleet-web-a task 5 round-1 review) wraps
	// just OUTSIDE withNodeRouter: withNodeRouter's own resolveMasterAndNodes
	// call needs the memo already present, and its request-cloning carries
	// this context value forward to the re-dispatched node-scoped request
	// automatically -- see withFleetMemo's own doc for why this is the
	// narrowest correct place to install it.
	return gzipMiddleware(securityHeaders(sessionMiddleware(sessions, userMiddleware(users, withFleetMemo(withNodeRouter(d, mux))))))
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
		// http.FileServer would answer with an index -- 404 them instead.
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		if ct := contentTypeByExt(r.URL.Path); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// Assets are addressed by content-hashed URLs (templates append
		// ?v=<hash> via the "asset" helper), so a given URL's bytes never
		// change -- cache them immutably. A new build changes the hash, hence
		// the URL, so browsers/CDN fetch the new asset instead of a stale one.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// contentTypeByExt returns the Content-Type this package's vendored/mockup
// assets and the Trinetra brand files (brand/*.png|.ico, fonts/*.woff2 and
// the font's OFL.txt) need, or "" to let http.FileServer's default
// sniffing/mime lookup decide (harmless for extensions this handler doesn't
// special-case). woff2 in particular is missing from many hosts' mime.types.
func contentTypeByExt(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".txt"):
		return "text/plain; charset=utf-8"
	}
	return ""
}

// enrollPageHandler renders the passkey-registration page (ported from
// ui-mockup/enroll.html -- see templates/enroll.html) through the bare/
// centered layout (base_bare.html/BarePageData, templates.go): unlike the
// dashboard/app-shell pages, there's no signed-in session yet to fill a
// sidebar/topbar with. assets/app.js wires the page's form to
// /enroll/begin and /enroll/finish via navigator.credentials.create.
//
// Any ?token=... on this GET is threaded through to BarePageData.EnrollToken
// (templates.go) so enroll.html can stash it in a hidden field and app.js
// can echo it back as /enroll/begin's "token" field -- this handler itself
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
// this page's own ?token= query parameter -- see enrollPageHandler).
type enrollBeginRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// enrollBeginHandler starts a WebAuthn registration ceremony (beginRegistration,
// auth_webauthn.go) for the posted name, creating a brand-new *User (not yet
// persisted -- finishRegistration's store.Put is what actually writes it).
//
// SECURITY: this endpoint is UNAUTHENTICATED, so it must ONLY ever create a
// new account -- it must never attach a credential to an existing one. An
// earlier version looked the name up with store.ByName and, on a match, ran
// the ceremony against the existing *User (with its existing role); that was
// a cross-account credential-injection / account-takeover bug (an anonymous
// caller could POST name="admin" and bind their own passkey to the admin
// account). So a name that already exists is rejected with 409 here.
// Adding a second passkey to an EXISTING account (multi-device) must
// instead go through an authenticated session (the account's own owner) or
// a future admin-managed flow -- never this anonymous path.
//
// Role assignment (issue #62, resolved): resolveEnrollRole
// (enroll_tokens.go) decides how the new account proceeds -- an admin-issued
// enrollment token's Role if one was posted (tokenStore.Redeem also
// enforces the token being unknown/expired/already-used, and burns the
// single-use token now), or a tokenless first-run BOOTSTRAP attempt when no
// account exists yet, or a flat refusal once any account already exists:
// unauthenticated open enrollment is only ever valid for that first
// account. For a bootstrap attempt the admin role is NOT assigned here --
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
			// endpoint -- see the SECURITY note above. Checked BEFORE any
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
// ui-mockup/login.html -- see templates/login.html) through the bare/
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
// credentials -- the login page's single "Continue with passkey" button
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
// regression, and -- only on success -- sets the sw_session cookie.
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

// logoutHandler deletes the caller's signed-in session (if any -- see
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
