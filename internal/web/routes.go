package web

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"
)

// newHandler builds the full ServeMux Start binds an http.Server around.
func newHandler(d Deps) http.Handler {
	assetsSub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// assetsFS is a compile-time //go:embed of a directory that exists in this package
		// (assets.go); fs.Sub can only fail here if that invariant is broken.
		panic("internal/web: assets embed missing \"assets\" dir: " + err.Error())
	}

	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assetHandler(assetsSub)))
	// / is the true landing route (public-rework task): an authenticated visitor (viewer or
	// admin) sees the dashboard, exactly like the old requireRole(RoleViewer, ...) wiring.
	mux.HandleFunc("GET /{$}", rootHandler(d))
	// /events: the SSE stream dashboard.html's live tiles/charts subscribe to (assets/app.js's
	// swBootSSE, sse.go).
	mux.HandleFunc("GET /events", requireRole(RoleViewer, d, eventsHandler(d)))
	// /history + /api/series: time-range history graphs backed by the daemon's SampleStore,
	// reached through Deps.API.Series (core.API, see server.go's Deps.API doc).
	mux.HandleFunc("GET /history", requireRole(RoleViewer, d, historyPageHandler(d)))
	mux.HandleFunc("GET /api/series", requireRole(RoleViewer, d, seriesAPIHandler(d)))
	mux.HandleFunc("GET /api/downtime", requireRole(RoleViewer, d, downtimeAPIHandler(d)))
	// /monitoring: the detailed per-entity view (containers/systemd units/
	// processes/filesystems).
	mux.HandleFunc("GET /monitoring", requireRole(RoleViewer, d, monitoringHandler(d)))
	// /host (#100): the static host hardware/OS inventory, read over the control socket via
	// Deps.API.HostInfo.
	mux.HandleFunc("GET /host", requireRole(RoleViewer, d, hostPageHandler(d)))
	// /api/container/logs (#115): a docker-logs snapshot for the dashboard
	// drawer's "View logs" action. Admin-gated -- logs can carry secrets.
	mux.HandleFunc("GET /api/container/logs", requireRole(RoleAdmin, d, containerLogsHandler(d)))
	// /fleet + /fleet/table + /api/fleet/nodes: the fleet overview -- health strip,
	// filterable/sortable node table, and its htmx poll fragment/JSON API.
	mux.HandleFunc("GET /fleet", requireRole(RoleViewer, d, fleetOverviewHandler(d)))
	mux.HandleFunc("GET /fleet/table", requireRole(RoleViewer, d, fleetTableHandler(d)))
	mux.HandleFunc("GET /api/fleet/nodes", requireRole(RoleViewer, d, fleetNodesAPIHandler(d)))
	// /fleet/compare: the fleet-wide metric compare view over FleetAPI.FleetSeries --
	// ?nodes=a,b,c (the table's "Compare" action below) or ?tag=web selects the nodes.
	mux.HandleFunc("GET /fleet/compare", requireRole(RoleViewer, d, fleetCompareHandler(d)))
	// /fleet/incidents + /fleet/incidents/{id} (+ /table poll fragment, + ack/silence
	// mutations).
	mux.HandleFunc("GET /fleet/incidents", requireRole(RoleViewer, d, fleetIncidentsHandler(d)))
	mux.HandleFunc("GET /fleet/incidents/table", requireRole(RoleViewer, d, fleetIncidentsTableHandler(d)))
	mux.HandleFunc("GET /fleet/incidents/{id}", requireRole(RoleViewer, d, fleetIncidentHandler(d)))
	mux.HandleFunc("POST /fleet/incidents/{id}/ack", fleetResponderMutation(d, fleetIncidentAckHandler(d)))
	mux.HandleFunc("POST /fleet/incidents/{id}/silence", fleetAdminMutation(d, fleetIncidentSilenceHandler(d)))
	// /fleet/alerting (+ /test, + /fleet/rules/state): the routing/escalation config editor
	// (routes/policies/rules, "edit as JSON"), the route tester, and the rule-state fragment.
	mux.HandleFunc("GET /fleet/alerting", requireRole(RoleViewer, d, fleetAlertingPageHandler(d)))
	mux.HandleFunc("POST /fleet/alerting", limitBody(alertingMaxBodyBytes, fleetAdminMutation(d, fleetAlertingSaveHandler(d))))
	mux.HandleFunc("POST /fleet/alerting/test", requireRole(RoleViewer, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(fleetAlertingTestHandler(d)).ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /fleet/rules/state", requireRole(RoleViewer, d, fleetRulesStateHandler(d)))
	// Public status page services admin (#157, handlers_statuspage_services.go).
	mux.HandleFunc("GET /status-page/services", requireRole(RoleAdmin, d, statusServicesPageHandler(d)))
	mux.HandleFunc("POST /status-page/services", fleetAdminMutation(d, statusServiceSaveHandler(d)))
	mux.HandleFunc("POST /status-page/services/add-targets", fleetAdminMutation(d, statusServiceAddTargetsHandler(d)))
	mux.HandleFunc("POST /status-page/services/{id}/delete", fleetAdminMutation(d, statusServiceDeleteHandler(d)))
	// Status-page incidents (#157, handlers_statuspage_incidents.go): responder+.
	mux.HandleFunc("GET /status-page/incidents", requireRole(RoleResponder, d, statusIncidentsPageHandler(d)))
	mux.HandleFunc("POST /status-page/incidents", fleetResponderMutation(d, statusIncidentCreateHandler(d)))
	mux.HandleFunc("GET /status-page/incidents/{id}", requireRole(RoleResponder, d, statusIncidentPageHandler(d)))
	mux.HandleFunc("POST /status-page/incidents/{id}/updates", fleetResponderMutation(d, statusUpdatePostHandler(d)))
	mux.HandleFunc("POST /status-page/incidents/{id}/updates/{uid}", fleetResponderMutation(d, statusUpdateEditHandler(d)))
	mux.HandleFunc("POST /status-page/incidents/{id}/edit", fleetResponderMutation(d, statusIncidentEditHandler(d)))
	mux.HandleFunc("POST /status-page/incidents/{id}/delete", fleetAdminMutation(d, statusIncidentDeleteHandler(d)))
	// /fleet/silences (+ /{id}/expire, + /fleet/maintenance + /{id}/delete).
	mux.HandleFunc("GET /fleet/silences", requireRole(RoleViewer, d, fleetSilencesPageHandler(d)))
	mux.HandleFunc("POST /fleet/silences", fleetAdminMutation(d, fleetSilenceCreateHandler(d)))
	mux.HandleFunc("POST /fleet/silences/{id}/expire", fleetAdminMutation(d, fleetSilenceExpireHandler(d)))
	mux.HandleFunc("POST /fleet/maintenance", fleetAdminMutation(d, fleetMaintenanceCreateHandler(d)))
	mux.HandleFunc("POST /fleet/maintenance/{id}/delete", fleetAdminMutation(d, fleetMaintenanceDeleteHandler(d)))
	// /fleet/admin + /fleet/tokens*/ + /fleet/nodes/*: node management
	// (rename/tags/revoke/remove) and join-token issuance/revocation.
	mux.HandleFunc("GET /fleet/admin", requireRole(RoleAdmin, d, fleetAdminPageHandler(d)))
	mux.HandleFunc("POST /fleet/tokens", fleetAdminMutation(d, fleetTokenCreateHandler(d)))
	mux.HandleFunc("POST /fleet/tokens/{id}/delete", fleetAdminMutation(d, fleetTokenDeleteHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/rename", fleetAdminMutation(d, fleetNodeRenameHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/tags", fleetAdminMutation(d, fleetNodeTagsHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/revoke", fleetAdminMutation(d, fleetNodeRevokeHandler(d)))
	mux.HandleFunc("POST /fleet/nodes/{id}/remove", fleetAdminMutation(d, fleetNodeRemoveHandler(d)))
	// /fleet/managed (+ /{id}/delete) + /fleet/audit: the managed-config fragment editor +
	// per-node status, and the fleet audit log.
	mux.HandleFunc("GET /fleet/managed", requireRole(RoleViewer, d, fleetManagedPageHandler(d)))
	mux.HandleFunc("POST /fleet/managed", fleetAdminMutation(d, fleetManagedSaveHandler(d)))
	mux.HandleFunc("POST /fleet/managed/{id}/delete", fleetAdminMutation(d, fleetManagedDeleteHandler(d)))
	mux.HandleFunc("GET /fleet/audit", requireRole(RoleAdmin, d, fleetAuditPageHandler(d)))
	// beginLimiter caps the unauthenticated ceremony-begin rate per client so an anonymous
	// caller can't hammer the shared ceremonies.json lock (#95).
	beginLimiter := newRateLimiter(beginRateMax, beginRateWindow)
	mux.HandleFunc("GET /enroll", enrollPageHandler(d))
	mux.HandleFunc("POST /enroll/begin", rateLimitBegin(beginLimiter, enrollBeginHandler(d)))
	mux.HandleFunc("POST /enroll/finish", enrollFinishHandler(d))
	mux.HandleFunc("GET /login", loginPageHandler(d))
	mux.HandleFunc("POST /login/begin", rateLimitBegin(beginLimiter, loginBeginHandler(d)))
	mux.HandleFunc("POST /login/finish", loginFinishHandler(d))
	// /logout is a signed-in session's own mutation (not a pre-auth ceremony endpoint like
	// /enroll or /login), so it's CSRF-protected -- see requireCSRF's doc.
	mux.Handle("POST /logout", requireCSRF(logoutHandler(d)))

	// Admin-only routes (config/channels/users/public-settings), gated by
	// requireRole(RoleAdmin, ...) (middleware.go). /config: the config editor -- thresholds.
	mux.HandleFunc("GET /config", requireRole(RoleAdmin, d, configPageHandler(d)))
	mux.HandleFunc("POST /config", configMutation(d, configSaveHandler(d)))
	// /updates: self-update status + manual actions over
	// core.API.UpdateStatus/UpdateCheck/UpdateApply/UpdateRollback.
	mux.HandleFunc("GET /updates", requireRole(RoleAdmin, d, updatesPageHandler(d)))
	mux.HandleFunc("POST /updates/check", updatesMutation(d, updatesCheckHandler(d)))
	mux.HandleFunc("POST /updates/apply", updatesMutation(d, updatesApplyHandler(d)))
	mux.HandleFunc("POST /updates/rollback", updatesMutation(d, updatesRollbackHandler(d)))
	// /channels: CRUD over config.Channels (table + add/edit modal).
	mux.HandleFunc("GET /channels", requireRole(RoleAdmin, d, channelsPageHandler(d)))
	mux.HandleFunc("POST /channels", channelsMutation(d, channelsAddHandler(d)))
	mux.HandleFunc("POST /channels/{name}/update", channelsMutation(d, channelsUpdateHandler(d)))
	mux.HandleFunc("POST /channels/{name}/remove", channelsMutation(d, channelsRemoveHandler(d)))
	mux.HandleFunc("POST /channels/{name}/test", channelsMutation(d, channelsTestHandler(d)))
	// /settings/public + /public + /public/events: the admin-curated exposure picker.
	mux.HandleFunc("GET /settings/public", requireRole(RoleAdmin, d, publicSettingsPageHandler(d)))
	mux.HandleFunc("POST /settings/public", publicSettingsMutation(d, publicSettingsSaveHandler(d)))
	mux.HandleFunc("GET /public", publicRouteRedirectHandler)
	mux.HandleFunc("GET /status", publicPageHandler(d))
	mux.HandleFunc("GET /public/events", publicEventsHandler(d))
	// Public status page extras (#157): anonymous like /public/events (no requireRole); each
	// handler 404s itself when public.enabled is false or no services are configured.
	mux.HandleFunc("GET /status/history", statusHistoryHandler(d))
	mux.HandleFunc("GET /status/feed.atom", statusFeedHandler(d))
	mux.HandleFunc("GET /status/api.json", statusAPIHandler(d))

	// /alerts: alert history + active alerts, both read over the control socket
	// (Deps.API.AlertHistory/ActiveAlerts), viewer+ per the design doc.
	mux.HandleFunc("GET /alerts", requireRole(RoleViewer, d, alertsPageHandler(d)))
	mux.HandleFunc("POST /alerts/{key}/ack", requireRole(RoleResponder, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(alertsAckHandler(d)).ServeHTTP(w, r)
	}))
	// /alerts/{key}/unack: the ack action's inverse, over core.API.UnackAlert.
	mux.HandleFunc("POST /alerts/{key}/unack", requireRole(RoleResponder, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(alertsUnackHandler(d)).ServeHTTP(w, r)
	}))

	// /users: the real user-management page -- list accounts, issue/re-issue enrollment
	// tokens, change roles, remove accounts, revoke individual passkeys.
	mux.HandleFunc("GET /users", requireRole(RoleAdmin, d, usersPageHandler(d)))
	mux.HandleFunc("POST /users/invite", usersMutation(d, usersInviteHandler(d)))
	mux.HandleFunc("POST /users/{id}/role", usersMutation(d, usersRoleHandler(d)))
	mux.HandleFunc("POST /users/{id}/remove", usersMutation(d, usersRemoveHandler(d)))
	mux.HandleFunc("POST /users/{id}/credentials/{credParam}/revoke", usersMutation(d, usersRevokeCredentialHandler(d)))

	// sessionMiddleware runs for every request so any handler/template can read the current
	// session (sessionFromContext) -- including requireCSRF above.
	sessions := newSessionStore(d.StateDir)
	users := newUserStore(d.StateDir)
	// gzipMiddleware is the outermost wrap: it compresses large responses (history/series
	// JSON) for congested uplinks, gated on Accept-Encoding: gzip and a 1KB minimum.
	return gzipMiddleware(securityHeaders(sessionMiddleware(sessions, userMiddleware(users, withFleetMemo(withNodeRouter(d, mux))))))
}

// assetHandler wraps http.FileServer to force a deterministic Content-Type for the
// extensions the shell needs.
func assetHandler(assets fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// After StripPrefix("/assets/"), "/assets/" arrives as "" and "/assets/sub/" as "sub/";
		// both are directory requests that http.FileServer would answer with an index.
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		if ct := contentTypeByExt(r.URL.Path); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		// Assets are addressed by content-hashed URLs (templates append ?v=<hash> via the "asset"
		// helper), so a given URL's bytes never change -- cache them immutably.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fileServer.ServeHTTP(w, r)
	})
}

// contentTypeByExt returns the Content-Type this package's vendored assets and the Trinetra
// brand files (brand/*.png|.ico, fonts/*.woff2 and the font's OFL.txt) need.
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

// enrollPageHandler renders the passkey-registration page (templates/ enroll.html) through
// the bare/centered layout (base_bare.html/BarePageData, templates.go).
func enrollPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newBarePageData(r, "Set up passkey")
		if data.EnrollToken == "" {
			empty, err := newUserStore(d.StateDir).IsEmpty()
			switch {
			case err != nil || !empty:
				data.EnrollClosed = true
			case !localOnly(d.Cfg()):
				data.EnrollNeedsSetup = true
			}
		}
		if err := renderBarePage(w, "enroll.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// enrollBeginRequest is POST /enroll/begin's JSON body: the account name typed into the
// enroll page's #enrollName input, plus an optional enrollment token.
type enrollBeginRequest struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// enrollBeginHandler starts a WebAuthn registration ceremony (beginRegistration,
// auth_webauthn.go) for the posted name, creating a brand-new *User.
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
			// Never attach to an existing account from this unauthenticated endpoint -- see the
			// SECURITY note above.
			http.Error(w, "an account with that name already exists; adding a passkey to an existing account will require an admin invite", http.StatusConflict)
			return
		}

		role, bootstrap, err := resolveEnrollRole(newTokenStore(d.StateDir), store, strings.TrimSpace(req.Token), localOnly(d.Cfg()))
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		id, err := newUserID()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// For a bootstrap (tokenless first-run) enrollment, role is left empty here on purpose:
		// finishRegistration -> CreateFirstAdmin assigns RoleAdmin atomically with the write.
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

// enrollFinishHandler completes the ceremony enrollBeginHandler started: it verifies the
// browser's attestation response.
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

// loginPageHandler renders the passkey sign-in page (templates/login.html) through the
// bare/centered layout, same as enrollPageHandler.
func loginPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newBarePageData(r, "Sign in")
		if err := renderBarePage(w, "login.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// loginBeginHandler starts a WebAuthn sign-in.
func loginBeginHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wa, err := webAuthnConfig(d.Cfg(), r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if r.ContentLength != 0 {
			_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
		}
		ceremonies := newCeremonyStore(d.StateDir)
		assertion, err := beginLoginFor(w, r, wa, ceremonies, newUserStore(d.StateDir), body.Name)
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

// loginFinishHandler completes the ceremony loginBeginHandler started: it verifies the
// browser's assertion response.
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

// logoutHandler deletes the caller's signed-in session.
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
