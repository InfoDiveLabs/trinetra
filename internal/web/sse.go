package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// sseDefaultInterval is eventsHandler's fallback per-connection ticker period when Deps.Cfg
// is nil or reports an invalid FastInterval.
const sseDefaultInterval = 5 * time.Second

// sseTickerInterval resolves the SSE stream's cadence to the daemon's current fast-tier
// interval.
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

// sseFallbackInterval is eventsHandler/publicEventsHandler's ticker cadence once a live
// event subscription is active (Deps.Subscribe != nil).
var sseFallbackInterval = 30 * time.Second

// writeSnapshotEvent JSON-encodes view as one SSE "snapshot" frame (event: snapshot / data:
// <json>) and flushes it immediately.
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

// writeAlertEvent JSON-encodes ev as one SSE "alert" frame (event: alert / data: <json>)
// and flushes it immediately.
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

// eventsHandler serves GET /events: a Server-Sent Events stream of the live DashboardView
// (Deps.Snapshot(), the same projection dashboardHandler's initial page render uses).
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

		// sub stays nil (and its select case below never fires, since a receive on a nil channel
		// blocks forever) unless Deps.Subscribe is set and succeeds.
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
					// The stream ended (daemon connection dropped, etc.): stop selecting on sub.
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

// remoteNodeSnapshot reads apiFor(r, d).Snapshot() for remoteNodeEventsLoop's polling path.
func remoteNodeSnapshot(r *http.Request, d Deps) (DashboardView, error) {
	api := apiFor(r, d)
	if api == nil {
		return DashboardView{}, errRemoteNodeAPIUnavailable
	}
	return api.Snapshot()
}

// errRemoteNodeAPIUnavailable is remoteNodeSnapshot's error for "there is no core.API to
// poll at all" (apiFor(r,d) returned nil).
var errRemoteNodeAPIUnavailable = fmt.Errorf("remote node API unavailable")

// remoteNodeStaleThreshold is how many CONSECUTIVE remoteNodeSnapshot failures
// remoteNodeEventsLoop tolerates before it tells the browser the stream is stale.
const remoteNodeStaleThreshold = 3

// staleEventData is the "stale" SSE event's JSON payload: the Unix-seconds time of the last
// successful snapshot.
type staleEventData struct {
	LastSuccess int64 `json:"last_success"`
}

// writeStaleEvent JSON-encodes a "stale" SSE frame and flushes it immediately --
// remoteNodeEventsLoop's counterpart to writeSnapshotEvent.
func writeStaleEvent(w http.ResponseWriter, f http.Flusher, lastSuccess time.Time) bool {
	b, err := json.Marshal(staleEventData{LastSuccess: lastSuccess.Unix()})
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "event: stale\ndata: %s\n\n", b); err != nil {
		return false
	}
	f.Flush()
	return true
}

// remoteNodeEventsLoop is eventsHandler's path for a request scoped to a remote fleet node
// (nodeFrom(r).Self == false).
func remoteNodeEventsLoop(w http.ResponseWriter, r *http.Request, flusher http.Flusher, d Deps) {
	last, err := remoteNodeSnapshot(r, d)
	if !writeSnapshotEvent(w, flusher, last) {
		return
	}
	lastGoodAt := time.Now()
	consecErrors := 0
	if err != nil {
		consecErrors = 1
	}

	ctx := r.Context()
	ticker := time.NewTicker(sseTickerInterval(d.Cfg))
	defer ticker.Stop()

	wasStale := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur, err := remoteNodeSnapshot(r, d)
			if err != nil {
				consecErrors++
				if consecErrors == remoteNodeStaleThreshold {
					if !writeStaleEvent(w, flusher, lastGoodAt) {
						return
					}
					wasStale = true
				}
				// Hold: last/lastGoodAt stay exactly as they were: never
				// overwrite a good snapshot with a failed poll's zero value.
				continue
			}
			consecErrors = 0
			lastGoodAt = time.Now()
			changed := !reflect.DeepEqual(cur, last)
			last = cur
			if !changed && !wasStale {
				continue
			}
			wasStale = false
			if !writeSnapshotEvent(w, flusher, cur) {
				return
			}
		}
	}
}

// publicSSEFrame is the ONLY payload GET /public/events ever marshals onto the wire:
// publicPanelView (Panels) is the exact tile shape /public's HTML renders.
type publicSSEFrame struct {
	Panels []publicPanelView `json:"panels"`
	// Availability is a pointer.
	Availability *Availability `json:"availability,omitempty"`
}

// buildPublicSSEFrame is GET /public/events' half of the SAME allowlist chokepoint
// /public's HTML uses (buildPublicPanels/publicPanelsContain, handlers_public.go).
func buildPublicSSEFrame(allowlist []string, snap DashboardView) publicSSEFrame {
	f := publicSSEFrame{Panels: buildPublicPanels(allowlist, snap)}
	if publicPanelsContain(allowlist, "availability") {
		av := snap.Availability
		f.Availability = &av
	}
	return f
}

// writePublicSnapshotEvent is writeSnapshotEvent's counterpart for the public stream's
// narrower payload type -- same "snapshot" event name.
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

// publicEventsHandler serves GET /public/events: the anonymous counterpart to /events
// (eventsHandler above) that backs /public's live resource tiles/availability strip.
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

		// See eventsHandler's identical sub/interval setup for why sub is left nil (disabling its
		// select case for good) unless Deps.Subscribe is set and succeeds.
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
					// Stream ended: stop selecting on sub (see eventsHandler's identical comment on why
					// this must happen, not just break out of this case) and fall back to the ticker.
					sub = nil
					continue
				}
				if ev.Kind != "snapshot" {
					// Never leak alert detail to an anonymous subscriber -- see this handler's doc.
					continue
				}
				// Re-check cfg.Public.Enabled on every push, same as every ticker tick below: an admin
				// disabling /public mid-stream must stop it promptly regardless of which case fired.
				if d.Cfg != nil && !d.Cfg().Public.Enabled {
					return
				}
				if !writePublicSnapshotEvent(w, flusher, frame()) {
					return
				}
			case <-ticker.C:
				// Re-check cfg.Public.Enabled on every tick (not just at connect time).
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
