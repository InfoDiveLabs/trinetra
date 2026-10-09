package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// nodeNotConnectedReason is the fixed UI string this package uses wherever a remote-node
// action is disabled/refused because the node's stream isn't currently connected.
const nodeNotConnectedReason = "node is not connected"

// containerLogsErrStatus maps a remote node's ContainerLogs error text to this endpoint's
// JSON status, matched by exact SUFFIX.
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

// containerLogsHandler serves GET /api/container/logs?name=<c>&tail=<n>: a plain-text
// snapshot of a docker container's recent logs (#115).
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
			// An unknown container / docker-unavailable is a client-facing 404, not a 500: it is an
			// ordinary outcome (container gone, no docker), and the message is safe.
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
