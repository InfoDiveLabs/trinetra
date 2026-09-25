package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// sseDefaultInterval is eventsHandler's fallback per-connection ticker
// period when Deps.Cfg is nil or reports an invalid FastInterval (defensive;
// every real Deps built by the trinetra-web binary's buildDeps
// (cmd/trinetra-web) always has a
// valid one) -- config.Default's own FastInterval (5s).
const sseDefaultInterval = 5 * time.Second

// sseTickerInterval resolves the SSE stream's cadence to the daemon's
// current fast-tier interval (cfg.FastInterval -- the same cadence
// snapshotHub itself is republished on, see
// internal/trinetra/snapshot_hub.go/daemon.go), so a subscriber never polls
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

// sseFallbackInterval is eventsHandler/publicEventsHandler's ticker cadence
// once a live event subscription is active (Deps.Subscribe != nil): the
// stream is push-driven at that point (bus.Publish -> control socket ->
// Deps.Subscribe's channel), so this ticker is no longer the primary refresh
// mechanism sseTickerInterval is for the pure-poll path -- it only exists as
// a coarse safety net that keeps a subscriber current if the stream stalls
// or the subscribe channel closes (see writeSnapshotEvent's callers below).
// A package var, not a const, so a test can shrink it rather than wait 30
// real seconds for a fallback tick.
var sseFallbackInterval = 30 * time.Second

// writeSnapshotEvent JSON-encodes view as one SSE "snapshot" frame (event:
// snapshot / data: <json>) and flushes it immediately, so the browser's
// EventSource dispatches it as soon as it's on the wire rather than sitting
// in a buffer. Returns false when the write itself failed (client
// disconnected -- the io.Writer level signal, complementing eventsHandler's
// r.Context().Done() check for the case a disconnect is noticed between
// writes rather than during one), telling the caller to stop the stream;
// a JSON marshal failure (shouldn't happen -- DashboardView is all
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

// writeAlertEvent JSON-encodes ev as one SSE "alert" frame (event: alert /
// data: <json>) and flushes it immediately -- eventsHandler's counterpart to
// writeSnapshotEvent for a non-"snapshot" LiveEvent pushed on Deps.Subscribe's
// channel (an alert fire/recover), so the browser can toast/refresh its
// alert list the moment the daemon's bus publishes it, rather than waiting
// for the next snapshot tick to notice a changed alert count. Same
// write/flush/false-on-write-failure contract as writeSnapshotEvent; this
// frame is never written on the public stream (see publicEventsHandler's
// doc for why alert detail must never reach an anonymous subscriber).
func writeAlertEvent(w http.ResponseWriter, f http.Flusher, ev LiveEvent) bool {
	b, err := json.Marshal(ev)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: alert\ndata: %s\n\n", b); err != nil {
		return false
	}
	f.Flush()
	return true
}

// eventsHandler serves GET /events: a Server-Sent Events stream of the live
// DashboardView (Deps.Snapshot(), the same projection dashboardHandler's
// initial page render uses) -- assets/app.js's swBootSSE consumes it to keep
// the dashboard's tiles/charts live between full page loads, per the design
// doc's "live snapshot sharing" architecture (no status.json polling, no
// second timer racing the daemon's own sampler loop).
//
// The very first frame is written immediately, before anything else, so a
// subscriber sees current data right away.
//
// As of Task 3, when Deps.Subscribe is set this handler is PUSH-driven: it
// opens one live channel at connection start (ctx = r.Context()) and reacts
// to whatever the daemon's event bus publishes instead of polling on a
// fixed cadence -- a Kind:"snapshot" LiveEvent triggers a fresh snapshot
// frame (writeSnapshotEvent), any other kind (an alert fire/recover) writes
// a distinct alert frame (writeAlertEvent) so the browser can toast/refresh
// its alert list the instant it happens. A coarse sseFallbackInterval ticker
// stays running throughout as a safety net (still calling snapshot()) in
// case the stream stalls, and once the channel closes (the daemon
// connection dropped) the handler falls back to that ticker for the rest of
// the connection rather than tearing the stream down.
//
// When Deps.Subscribe is nil (e.g. a test, or a backend with no live
// daemon), eventsHandler keeps its original pure-ticker behavior unchanged:
// poll Deps.Snapshot() on sseTickerInterval, nothing else.
//
// As of Task 4 (fleet-web-a), a request scoped to a remote fleet node
// (nodeFrom(r).Self == false -- reachable once withNodeRouter stopped
// excluding /events from node routing, node_scope.go) never reaches any of
// the self-scope logic below at all: it is handed off to
// remoteNodeEventsLoop instead, a poll-only path over apiFor(r,d).Snapshot()
// that never calls Deps.Subscribe (there is no per-node live push over the
// control socket -- Subscribe is scoped to THIS daemon's own event bus, not
// a remote node's).
//
// Either way, the stream loops on a select that also watches
// r.Context().Done(): a client disconnect (navigating away, closing the
// tab, the browser's own EventSource reconnect logic tearing down the old
// connection) cancels the request context, and this handler notices and
// returns promptly -- it does not wait for the next tick's write to fail
// before giving up the goroutine/ticker/subscription.
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

		if !nodeFrom(r).Self {
			remoteNodeEventsLoop(w, r, flusher, d)
			return
		}

		snapshot := func() DashboardView {
			if d.Snapshot == nil {
				return DashboardView{}
			}
			return d.Snapshot()
		}

		if !writeSnapshotEvent(w, flusher, snapshot()) {
			return
		}

		ctx := r.Context()

		// sub stays nil (and its select case below never fires, since a
		// receive on a nil channel blocks forever) unless Deps.Subscribe is
		// set and succeeds -- the two conditions under which this handler
		// must behave exactly like the pre-Task-3 pure-ticker path.
		var sub <-chan LiveEvent
		interval := sseTickerInterval(d.Cfg)
		if d.Subscribe != nil {
			if s, err := d.Subscribe(ctx); err == nil {
				sub = s
				interval = sseFallbackInterval
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					// The stream ended (daemon connection dropped, etc.):
					// stop selecting on sub (nil disables the case for
					// good -- otherwise a closed channel would fire this
					// case on every loop iteration and busy-spin) and rely
					// on the fallback ticker for the rest of the
					// connection.
					sub = nil
					continue
				}
				if ev.Kind == "snapshot" {
					if !writeSnapshotEvent(w, flusher, snapshot()) {
						return
					}
				} else if !writeAlertEvent(w, flusher, ev) {
					return
				}
			case <-ticker.C:
				if !writeSnapshotEvent(w, flusher, snapshot()) {
					return
				}
			}
		}
	}
}

// remoteNodeSnapshot reads apiFor(r, d).Snapshot() for
// remoteNodeEventsLoop's polling path, collapsing a nil API or a read error
// to the zero DashboardView -- the same degrade-quietly contract
// buildDashboardPageData's own apiFor(r,d).Snapshot() call uses for a node
// page's initial render (handlers_dashboard.go), so a transient
// control-plane hiccup on the remote node shows a stale-but-present frame
// rather than tearing the stream down.
func remoteNodeSnapshot(r *http.Request, d Deps) DashboardView {
	api := apiFor(r, d)
	if api == nil {
		return DashboardView{}
	}
	v, err := api.Snapshot()
	if err != nil {
		return DashboardView{}
	}
	return v
}

// remoteNodeEventsLoop is eventsHandler's path for a request scoped to a
// remote fleet node (nodeFrom(r).Self == false, Task 4/fleet-web-a): it
// polls apiFor(r,d).Snapshot() on sseTickerInterval and writes a fresh
// "snapshot" frame only when the polled view actually changed since the
// last one written (reflect.DeepEqual -- DashboardView carries slice
// fields, so a plain == comparison doesn't compile), rather than resending
// an identical frame on every tick. It never touches Deps.Subscribe: there
// is no per-node live push over the control socket yet (Subscribe is
// scoped to THIS daemon's own event bus, not a remote node's), so a remote
// alert fire/recover is never streamed here -- only snapshot polling, per
// the brief.
//
// The very first frame is always written immediately regardless of
// "changed", mirroring eventsHandler's own self-scope contract (a
// subscriber sees current data right away). Like eventsHandler's main
// loop, it watches r.Context().Done() so a client disconnect is noticed
// promptly rather than only on the next tick's failed write.
func remoteNodeEventsLoop(w http.ResponseWriter, r *http.Request, flusher http.Flusher, d Deps) {
	last := remoteNodeSnapshot(r, d)
	if !writeSnapshotEvent(w, flusher, last) {
		return
	}

	ctx := r.Context()
	ticker := time.NewTicker(sseTickerInterval(d.Cfg))
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := remoteNodeSnapshot(r, d)
			if reflect.DeepEqual(cur, last) {
				continue
			}
			last = cur
			if !writeSnapshotEvent(w, flusher, cur) {
				return
			}
		}
	}
}

// publicSSEFrame is the ONLY payload GET /public/events ever marshals onto
// the wire: publicPanelView (Panels) is the exact tile shape /public's HTML
// renders, and Availability is nil unless "availability" is in
// cfg.Public.Panels -- never a bare DashboardView. See buildPublicSSEFrame.
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
// handlers_public.go) -- it is the ONLY function that turns a snapshot into
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
// public stream's narrower payload type -- same "snapshot" event name (so
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
// tiles/availability strip -- see routes.go's wiring for why this is a
// wholly separate handler rather than a shared one.
//
// SECURITY:
//   - cfg.Public.Enabled=false 404s exactly like publicPageHandler, before
//     any streaming setup (headers, flusher check) even happens -- an
//     anonymous prober gets no signal beyond "not found" either way.
//   - Every frame is built by buildPublicSSEFrame(cfg.Public.Panels, ...),
//     re-reading BOTH the config and the snapshot on every tick (not just
//     the first frame) -- so an admin narrowing/disabling public.panels
//     mid-connection takes effect on the very next tick, not just for
//     brand-new connections; see TestPublicEventsStreamStopsOnDisableMidStream.
//   - Cache-Control: no-store (not eventsHandler's no-cache) -- matching
//     publicPageHandler's rationale (issue #67 follow-up): a caching
//     proxy/CDN must never keep serving stream frames after the admin
//     disables /public or narrows its allowlist.
//   - No session is read, and no cookie is ever set.
//   - As of Task 3, when Deps.Subscribe is set this stream is push-driven
//     the same way eventsHandler's is, but Kind:"snapshot" is the ONLY
//     LiveEvent kind that ever produces a frame here: an alert event (Title/
//     Source/Severity) is deliberately ignored rather than forwarded, since
//     unlike the authed /events stream, /public/events must never leak alert
//     detail to an anonymous subscriber (see TestPublicEventsSubscribeIgnoresAlertEvents).
//     A coarse sseFallbackInterval ticker (same rationale as eventsHandler's)
//     stays running as a safety net, and once the channel closes this
//     handler falls back to it for the rest of the connection.
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

		ctx := r.Context()

		// See eventsHandler's identical sub/interval setup for why sub is
		// left nil (disabling its select case for good) unless Deps.Subscribe
		// is set and succeeds.
		var sub <-chan LiveEvent
		interval := sseTickerInterval(d.Cfg)
		if d.Subscribe != nil {
			if s, err := d.Subscribe(ctx); err == nil {
				sub = s
				interval = sseFallbackInterval
			}
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					// Stream ended: stop selecting on sub (see
					// eventsHandler's identical comment on why this must
					// happen, not just break out of this case) and fall
					// back to the ticker.
					sub = nil
					continue
				}
				if ev.Kind != "snapshot" {
					// Never leak alert detail to an anonymous subscriber --
					// see this handler's doc.
					continue
				}
				// Re-check cfg.Public.Enabled on every push, same as every
				// ticker tick below: an admin disabling /public mid-stream
				// must stop it promptly regardless of which case fired.
				if d.Cfg != nil && !d.Cfg().Public.Enabled {
					return
				}
				if !writePublicSnapshotEvent(w, flusher, frame()) {
					return
				}
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
