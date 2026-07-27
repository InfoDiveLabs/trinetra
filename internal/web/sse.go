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

// publicSSEFrame is the ONLY payload GET /public/events ever marshals onto
// the wire: publicPanelView (Panels) is the exact tile shape /public's HTML
// renders, and Availability is nil unless "availability" is in
// cfg.Public.Panels — never a bare DashboardView. See buildPublicSSEFrame.
type publicSSEFrame struct {
	Panels []publicPanelView `json:"panels"`
	// Availability is a pointer (omitempty) rather than a zero-valued
	// struct so an allowlist that doesn't include "availability" omits the
	// field entirely from the JSON rather than sending an empty-but-present
	// one a client might mistake for "no downtime data yet".
	Availability *Availability `json:"availability,omitempty"`
}

// buildPublicSSEFrame is GET /public/events' half of the SAME allowlist
// chokepoint /public's HTML uses (buildPublicPanels/publicPanelsContain,
// handlers_public.go) — it is the ONLY function that turns a snapshot into
// this stream's wire payload, and it only ever does so by iterating
// allowlist, exactly like the page render. A panel id absent from allowlist
// is never looked at here, so it can never reach an anonymous subscriber
// over this stream any more than it can over the HTML.
func buildPublicSSEFrame(allowlist []string, snap DashboardView) publicSSEFrame {
	f := publicSSEFrame{Panels: buildPublicPanels(allowlist, snap)}
	if publicPanelsContain(allowlist, "availability") {
		av := snap.Availability
		f.Availability = &av
	}
	return f
}

// writePublicSnapshotEvent is writeSnapshotEvent's counterpart for the
// public stream's narrower payload type — same "snapshot" event name (so
// assets/app.js's swBootPublicSSE can use the same EventSource
// addEventListener("snapshot", ...) shape as the authed dashboard's
// swBootSSE), same flush-immediately/false-on-write-failure contract.
func writePublicSnapshotEvent(w http.ResponseWriter, f http.Flusher, frame publicSSEFrame) bool {
	b, err := json.Marshal(frame)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
		return false
	}
	f.Flush()
	return true
}

// publicEventsHandler serves GET /public/events: the anonymous counterpart
// to /events (eventsHandler above) that backs /public's live resource
// tiles/availability strip — see routes.go's wiring for why this is a
// wholly separate handler rather than a shared one.
//
// SECURITY:
//   - cfg.Public.Enabled=false 404s exactly like publicPageHandler, before
//     any streaming setup (headers, flusher check) even happens — an
//     anonymous prober gets no signal beyond "not found" either way.
//   - Every frame is built by buildPublicSSEFrame(cfg.Public.Panels, ...),
//     re-reading BOTH the config and the snapshot on every tick (not just
//     the first frame) — so an admin narrowing/disabling public.panels
//     mid-connection takes effect on the very next tick, not just for
//     brand-new connections; see TestPublicEventsStreamStopsOnDisableMidStream.
//   - Cache-Control: no-store (not eventsHandler's no-cache) — matching
//     publicPageHandler's rationale (issue #67 follow-up): a caching
//     proxy/CDN must never keep serving stream frames after the admin
//     disables /public or narrows its allowlist.
//   - No session is read, and no cookie is ever set.
func publicEventsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if d.Cfg == nil || !d.Cfg().Public.Enabled {
			http.NotFound(w, r)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		frame := func() publicSSEFrame {
			cfg := d.Cfg()
			var snap DashboardView
			if d.Snapshot != nil {
				snap = d.Snapshot()
			}
			var allowlist []string
			if cfg != nil {
				allowlist = cfg.Public.Panels
			}
			return buildPublicSSEFrame(allowlist, snap)
		}

		if !writePublicSnapshotEvent(w, flusher, frame()) {
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
				// Re-check cfg.Public.Enabled on every tick (not just at
				// connect time): an admin disabling /public mid-connection
				// must stop this stream promptly rather than keep pushing
				// frames to an anonymous subscriber the admin just turned
				// off.
				if d.Cfg != nil && !d.Cfg().Public.Enabled {
					return
				}
				if !writePublicSnapshotEvent(w, flusher, frame()) {
					return
				}
			}
		}
	}
}
