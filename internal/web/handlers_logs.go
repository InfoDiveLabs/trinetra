package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// nodeNotConnectedReason is the fixed UI string this package uses wherever a
// remote-node action is disabled/refused because the node's stream isn't
// currently connected. It deliberately matches internal/trinetra/fleet_rpc.go's
// errNodeNotConnected wording ("node is not connected") on purpose --
// internal/web cannot import internal/trinetra (that would invert the module's
// dependency direction), so this is a hardcoded literal kept in sync with that
// package's own text by convention/tests rather than by importing it, but
// there's no reason for the two to say anything different: both describe
// exactly the same condition.
const nodeNotConnectedReason = "node is not connected"

// containerLogsErrStatus maps a remote node's ContainerLogs error text to
// this endpoint's JSON status, matched by exact SUFFIX -- the same approach
// internal/control/client.go's reconstructWireErr uses to recover a core
// sentinel from a wire error's text, since replicaAPI.ContainerLogs
// (internal/trinetra/fleet_replica.go) and the RPC layer underneath it
// (fleet_rpc.go/fleet_rpc_child.go) only ever produce these three fixed
// strings for a stream/RPC-level failure:
//   - "node is not connected"    -> 503 (the stream itself is down)
//   - "node did not answer in 10s" -> 504 (the RPC call timed out)
//   - "node busy"                -> 429 (the child rejected a concurrent RPC)
//
// Anything else (a self-scope docker error, an unknown container name, the
// child's own reported error via the RPC's OK:false path) doesn't match any
// suffix and falls through to fixed=false, so the caller keeps the
// pre-existing plain-text 404 behavior for those.
func containerLogsErrStatus(err error) (status int, fixed bool) {
	msg := err.Error()
	switch {
	case strings.HasSuffix(msg, "node is not connected"):
		return http.StatusServiceUnavailable, true
	case strings.HasSuffix(msg, "node did not answer in 10s"):
		return http.StatusGatewayTimeout, true
	case strings.HasSuffix(msg, "node busy"):
		return http.StatusTooManyRequests, true
	}
	return http.StatusNotFound, false
}

// containerLogsHandler serves GET /api/container/logs?name=<c>&tail=<n>: a
// plain-text snapshot of a docker container's recent logs (#115), for the
// dashboard drawer's "View logs" action. Admin-gated (see routes.go) because
// container logs can carry secrets. The daemon validates name against the live
// container list before shelling out (see trinetra.collectContainerLogs), so
// this handler forwards the query verbatim and lets that layer refuse an
// unknown or malformed name.
//
// A request scoped to a remote fleet node (node_scope.go) goes through
// apiFor(r, d) like every other node-scoped GET, which resolves to the node's
// replicaAPI and runs the "container_logs" RPC over the master-to-child stream
// (fleet_replica.go/fleet_rpc.go). A stream/RPC-level failure is reported as
// JSON with a specific status (containerLogsErrStatus above); any other
// error (self-scope docker error, unknown container) is a plain-text 404.
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
		api := apiFor(r, d)
		if api == nil {
			http.Error(w, "logs unavailable", http.StatusServiceUnavailable)
			return
		}
		logs, err := api.ContainerLogs(name, tail)
		if err != nil {
			if status, fixed := containerLogsErrStatus(err); fixed {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
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
