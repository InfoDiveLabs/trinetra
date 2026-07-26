//go:build web

package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"serverwatch/internal/config"
)

// sseDefaultInterval is eventsHandler's fallback per-connection ticker
// period when Deps.Cfg is nil or reports an invalid FastInterval (defensive;
// every real Deps from internal/serverwatch/daemon_web.go always has a
// valid one) — config.Default's own FastInterval (5s).
const sseDefaultInterval = 5 * time.Second

// sseTickerInterval resolves the SSE stream's cadence to the daemon's
// current fast-tier interval (cfg.FastInterval — the same cadence
// snapshotHub itself is republished on, see
// internal/serverwatch/web_deps.go/daemon.go), so a subscriber never polls
// faster than the source actually changes.
func sseTickerInterval(cfg func() *config.Config) time.Duration {
	if cfg == nil {
		return sseDefaultInterval
	}
	c := cfg()
	if c == nil || c.FastInterval <= 0 {
		return sseDefaultInterval
	}
	return time.Duration(c.FastInterval) * time.Second
}

// writeSnapshotEvent JSON-encodes view as one SSE "snapshot" frame (event:
// snapshot / data: <json>) and flushes it immediately, so the browser's
// EventSource dispatches it as soon as it's on the wire rather than sitting
// in a buffer. Returns false when the write itself failed (client
// disconnected — the io.Writer level signal, complementing eventsHandler's
// r.Context().Done() check for the case a disconnect is noticed between
// writes rather than during one), telling the caller to stop the stream;
// a JSON marshal failure (shouldn't happen — DashboardView is all
// plain data) just skips that one frame and keeps the connection open.
func writeSnapshotEvent(w http.ResponseWriter, f http.Flusher, view DashboardView) bool {
	b, err := json.Marshal(view)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
		return false
	}
	f.Flush()
	return true
}

// eventsHandler serves GET /events: a Server-Sent Events stream of the live
// DashboardView (Deps.Snapshot(), the same projection dashboardHandler's
// initial page render uses) on the fast-tier cadence
// (sseTickerInterval) — assets/app.js's swBootSSE consumes it to keep the
// dashboard's tiles/charts live between full page loads, per the design
// doc's "live snapshot sharing" architecture (no status.json polling, no
// second timer racing the daemon's own sampler loop).
//
// The very first frame is written immediately, before the ticker's first
// tick, so a subscriber sees current data right away rather than waiting up
// to one full interval for it. The stream then loops on a select between
// the ticker and r.Context().Done(): a client disconnect (navigating away,
// closing the tab, the browser's own EventSource reconnect logic tearing
// down the old connection) cancels the request context, and this handler
// notices and returns promptly — it does not wait for the next tick's write
// to fail before giving up the goroutine/ticker.
func eventsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		snapshot := func() DashboardView {
			if d.Snapshot == nil {
				return DashboardView{}
			}
			return d.Snapshot()
		}

		if !writeSnapshotEvent(w, flusher, snapshot()) {
			return
		}

		ticker := time.NewTicker(sseTickerInterval(d.Cfg))
		defer ticker.Stop()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !writeSnapshotEvent(w, flusher, snapshot()) {
					return
				}
			}
		}
	}
}
