//go:build web

package web

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// maxSeriesRangeSeconds bounds a single /api/series request's [from, to]
// span: 400 days, generously beyond the 30-day rollup-retention default
// (docs/DESIGN-storage.md) so any legitimate history query (even the
// mockup's widest "30d" chip) fits comfortably, while still rejecting the
// "absurd range" case this task's validation requires (e.g. from=0, a
// multi-century span) before it ever reaches the store.
const maxSeriesRangeSeconds = int64(400 * 24 * 3600)

// seriesResponse is GET /api/series's JSON body: Series is uPlot's own data
// shape — parallel arrays, [0] the x/timestamp axis and [1..] the y series
// (avg, then min, then max here) — so assets/app.js can hand it to
// `new uPlot(opts, data)`/`u.setData(data)` with no reshaping.
type seriesResponse struct {
	Metric string      `json:"metric"`
	Series [][]float64 `json:"series"`
}

// emptySeriesResponse is what an unknown metric, a nil Deps.Store, or a
// Query error all render as: four empty arrays (ts/avg/min/max) rather than
// omitting Series or erroring — see seriesAPIHandler's doc for why none of
// those cases is a 500.
func emptySeriesResponse(metric string) seriesResponse {
	return seriesResponse{Metric: metric, Series: [][]float64{{}, {}, {}, {}}}
}

// parseSeriesRange validates GET /api/series's from/to query parameters:
// both must parse as Unix-seconds integers, from must be strictly less than
// to, and the span must not exceed maxSeriesRangeSeconds. Returns ok=false
// (caller responds 400) on any failure — this is the only input validation
// /api/series does; the metric parameter itself is deliberately unchecked
// against an allowlist (see seriesAPIHandler's doc: an unrecognized metric
// is a normal "no data" outcome, not a validation error).
func parseSeriesRange(r *http.Request) (from, to int64, ok bool) {
	q := r.URL.Query()
	from, errFrom := strconv.ParseInt(q.Get("from"), 10, 64)
	to, errTo := strconv.ParseInt(q.Get("to"), 10, 64)
	if errFrom != nil || errTo != nil {
		return 0, 0, false
	}
	if from >= to {
		return 0, 0, false
	}
	if to-from > maxSeriesRangeSeconds {
		return 0, 0, false
	}
	return from, to, true
}

// seriesAPIHandler serves GET /api/series?metric=&from=&to=: the JSON feed
// templates/history.html's uPlot charts (assets/app.js's
// swBootHistoryCharts) fetch per metric/time-range-chip selection.
// requireRole(RoleViewer, ...) (routes.go's wiring) has already gated this
// by the time it runs.
//
// Three distinct "no real data" cases all render as a 200 empty
// seriesResponse rather than an error status, per this task's validation
// contract:
//   - Deps.Store is nil (no sample store configured for this daemon).
//   - The store returns an error for this metric/range (a query against a
//     metric name that isn't tracked at all behaves this way in both the
//     memStore and tsfile backends: no matching series, no error) — treated
//     the same as "no data" rather than surfaced as 500, since a client
//     picking a metric this daemon's config doesn't collect is an ordinary,
//     expected outcome (e.g. no "temp" sensor found on this host), not a
//     server fault.
//   - The store has no points at all for the metric (a real metric that
//     simply hasn't reported yet, or an unrecognized name).
//
// Only the from/to range itself is validated as a hard client error (400):
// unparseable, from>=to, or an absurdly wide span (parseSeriesRange).
func seriesAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metric := r.URL.Query().Get("metric")
		from, to, ok := parseSeriesRange(r)
		if !ok {
			http.Error(w, "invalid from/to range", http.StatusBadRequest)
			return
		}

		resp := emptySeriesResponse(metric)
		if d.Store != nil {
			if pts, err := d.Store.Query(metric, from, to); err == nil && len(pts) > 0 {
				ts := make([]float64, len(pts))
				avg := make([]float64, len(pts))
				mn := make([]float64, len(pts))
				mx := make([]float64, len(pts))
				for i, p := range pts {
					ts[i] = float64(p.TS)
					avg[i] = p.Avg
					mn[i] = p.Min
					mx[i] = p.Max
				}
				resp.Series = [][]float64{ts, avg, mn, mx}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// historyPageHandler renders GET /history: ported from
// ui-mockup/history.html (templates/history.html) through the full
// app-shell layout, same as dashboardHandler. The page itself carries no
// server-rendered chart data — assets/app.js's swBootHistoryCharts fetches
// every chart's points from /api/series client-side once the page loads,
// driven by the metric/time-range chips' data attributes.
func historyPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := newPageData(r, "History", "Metrics & downtime", "ok")
		if err := renderPage(w, "history.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
