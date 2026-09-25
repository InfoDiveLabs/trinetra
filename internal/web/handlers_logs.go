package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// remoteNodeUnavailableReason is the one shared UI-facing string for "this
// action isn't routed to a remote fleet node" (global-constraints.md: every
// per-node write action -- container logs, alert ack/unack, doctor -- is
// shown disabled with this reason rather than failing). It is deliberately
// not the underlying daemon's own errRemoteNode text ("not available for a
// remote fleet node yet", internal/trinetra/fleet_replica.go): both
// containerLogsHandler (below) and the alerts page (handlers_alerts.go's
// AlertsPageData.RemoteReason, rendered by templates/alerts.html) return/
// render this exact constant instead of a second, independently-typed
// literal, so the wording can't drift between the two call sites (task 3
// carry-over from the round-1 Task 2 review).
const remoteNodeUnavailableReason = "not available for a remote node yet"

// containerLogsHandler serves GET /api/container/logs?name=<c>&tail=<n>: a
// plain-text snapshot of a docker container's recent logs (#115), for the
// dashboard drawer's "View logs" action. Admin-gated (see routes.go) because
// container logs can carry secrets. The daemon validates name against the live
// container list before shelling out (see trinetra.collectContainerLogs), so
// this handler forwards the query verbatim and lets that layer refuse an
// unknown or malformed name.
//
// A request scoped to a remote fleet node (node_scope.go) never reaches the
// daemon at all: it gets a fixed 409 JSON error immediately (see
// remoteNodeUnavailableReason) rather than depending on whatever error text
// the node's own replica API happens to return.
func containerLogsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		if name == "" {
			http.Error(w, "missing container name", http.StatusBadRequest)
			return
		}
		tail := 200
		if t := r.URL.Query().Get("tail"); t != "" {
			n, err := strconv.Atoi(t)
			if err != nil || n <= 0 || n > 5000 {
				http.Error(w, "invalid tail", http.StatusBadRequest)
				return
			}
			tail = n
		}
		if !nodeFrom(r).Self {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": remoteNodeUnavailableReason})
			return
		}
		if d.API == nil {
			http.Error(w, "logs unavailable", http.StatusServiceUnavailable)
			return
		}
		logs, err := d.API.ContainerLogs(name, tail)
		if err != nil {
			// An unknown container / docker-unavailable is a client-facing 404,
			// not a 500: it is an ordinary outcome (container gone, no docker),
			// and the message is safe (it names only what the caller asked for).
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if _, err := w.Write([]byte(logs)); err != nil {
			return
		}
	}
}
