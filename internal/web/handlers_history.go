//go:build web

package web

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
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
			pts, err := d.Store.Query(metric, from, to)
			switch {
			case err != nil:
				// A genuine storage fault still renders as an empty 200 (the
				// UI stays resilient — a picked metric this daemon can't serve
				// is an ordinary outcome, not a page-breaking error), but log
				// it server-side so a real storage-layer failure isn't
				// operationally invisible. An unrecognized metric is NOT an
				// error in either backend (memStore/tsfile return an empty
				// series, no error), so this path only fires on an actual fault.
				log.Printf("web: /api/series query metric=%q from=%d to=%d: %v", metric, from, to, err)
			case len(pts) > 0:
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

// downtimeResponse is GET /api/downtime's JSON body: the downtime events
// overlapping the requested range, for templates/history.html's
// "Downtime · 30d" timeline/rows (assets/app.js's swBootHistoryCharts).
type downtimeResponse struct {
	Events []DownEventView `json:"events"`
}

// downtimeAPIHandler serves GET /api/downtime?from=&to=: the downtime-event
// feed the history page's "Downtime · 30d" panel fetches. Same validation
// and graceful-degradation contract as seriesAPIHandler — from/to validated
// as a hard 400 (parseSeriesRange), a nil Deps.Events or an events-store
// error both render as an empty 200 (the latter logged server-side) rather
// than a 500. requireRole(RoleViewer, ...) (routes.go) has already gated it.
func downtimeAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseSeriesRange(r)
		if !ok {
			http.Error(w, "invalid from/to range", http.StatusBadRequest)
			return
		}

		resp := downtimeResponse{Events: []DownEventView{}}
		if d.Events != nil {
			evs, err := d.Events.Events(from, to)
			if err != nil {
				log.Printf("web: /api/downtime query from=%d to=%d: %v", from, to, err)
			} else if len(evs) > 0 {
				resp.Events = evs
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// HistoryPageData is what templates/history.html renders against: the shared
// PageData plus the current filesystem mounts (from the live snapshot) the
// "Disk usage" panel graphs one series per — the mounts have to be resolved
// server-side because internal/web can't enumerate the store's metric names,
// and the live DashboardView (Deps.Snapshot) is the same mount list the
// dashboard's filesystems table already uses.
type HistoryPageData struct {
	PageData
	// DiskMounts is the sorted mount paths (DashboardView.Disks) the disk
	// panel graphs; empty when no filesystems are known (renders a note).
	DiskMounts []string
	// DiskMetrics is DiskMounts as the comma-joined "disk:<mount>" metric
	// list history.html hands the disk chart's data-metrics attribute, and
	// DiskLabels the parallel comma-joined mount labels for its legend.
	DiskMetrics string
	DiskLabels  string
}

// historyPageHandler renders GET /history: ported from
// ui-mockup/history.html (templates/history.html) through the full
// app-shell layout, same as dashboardHandler. The metric charts carry no
// server-rendered points — assets/app.js's swBootHistoryCharts fetches them
// from /api/series (and downtime from /api/downtime) client-side once the
// page loads, driven by the metric/time-range chips' data attributes. The
// one thing resolved server-side is the disk panel's per-mount series list,
// since internal/web can't enumerate the store's metric names itself.
func historyPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mounts := historyDiskMounts(d)
		metrics := make([]string, len(mounts))
		for i, m := range mounts {
			metrics[i] = "disk:" + m
		}
		data := HistoryPageData{
			PageData:    newPageData(r, d, "History", "Metrics & downtime"),
			DiskMounts:  mounts,
			DiskMetrics: strings.Join(metrics, ","),
			DiskLabels:  strings.Join(mounts, ","),
		}
		if err := renderHistoryPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// renderHistoryPage renders templates/history.html through the full
// app-shell layout (base.html) against HistoryPageData — the same
// parse/execute shape renderDashboardPage (handlers_dashboard.go) uses,
// mirrored here because this page needs HistoryPageData's extra disk-mount
// fields alongside the shared PageData ones.
func renderHistoryPage(w http.ResponseWriter, data HistoryPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/history.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// historyDiskMounts returns the current filesystem mounts (DashboardView.
// Disks, already sorted by mount by the daemon_web.go adapter) for the disk
// panel's per-mount series, or nil when there's no snapshot/no disks.
func historyDiskMounts(d Deps) []string {
	if d.Snapshot == nil {
		return nil
	}
	view := d.Snapshot()
	if len(view.Disks) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(view.Disks))
	for _, disk := range view.Disks {
		mounts = append(mounts, disk.Mount)
	}
	return mounts
}
