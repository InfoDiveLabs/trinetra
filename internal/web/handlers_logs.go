package web

import (
	"net/http"
	"strconv"
)

// containerLogsHandler serves GET /api/container/logs?name=<c>&tail=<n>: a
// plain-text snapshot of a docker container's recent logs (#115), for the
// dashboard drawer's "View logs" action. Admin-gated (see routes.go) because
// container logs can carry secrets. The daemon validates name against the live
// container list before shelling out (see serverwatch.collectContainerLogs), so
// this handler forwards the query verbatim and lets that layer refuse an
// unknown or malformed name.
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
