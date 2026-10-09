package web

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// maxSeriesRangeSeconds bounds a single /api/series request's [from, to] span: 400 days,
// generously beyond the 30-day rollup-retention default.
const maxSeriesRangeSeconds = int64(400 * 24 * 3600)

// seriesResponse is GET /api/series's JSON body: Series is uPlot's own data
// shape -- parallel arrays, [0] the x/timestamp axis and [1..] the y series
// (avg, then min, then max here) -- so assets/app.js can hand it to
// `new uPlot(opts, data)`/`u.setData(data)` with no reshaping.
type seriesResponse struct {
	Metric string      `json:"metric"`
	Series [][]float64 `json:"series"`
}

// emptySeriesResponse is what an unknown metric, a nil Deps.API, or a Series error all
// render as: four empty arrays (ts/avg/min/max) rather than omitting Series or erroring.
func emptySeriesResponse(metric string) seriesResponse {
	return seriesResponse{Metric: metric, Series: [][]float64{{}, {}, {}, {}}}
}

// parseSeriesRange validates GET /api/series's from/to query parameters: both must parse as
// Unix-seconds integers.
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
// templates/history.html's uPlot charts.
func seriesAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		metric := r.URL.Query().Get("metric")
		from, to, ok := parseSeriesRange(r)
		if !ok {
			http.Error(w, "invalid from/to range", http.StatusBadRequest)
			return
		}

		resp := emptySeriesResponse(metric)
		if api := apiFor(r, d); api != nil {
			pts, err := api.Series(metric, from, to, core.ResAuto)
			switch {
			case err != nil:
				// A genuine storage fault still renders as an empty 200 (the
				// UI stays resilient -- a picked metric this daemon can't serve
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

// downtimeDefaultLimit and downtimeMaxLimit bound GET /api/downtime's `limit` query
// parameter (parseDowntimePagination): unset defaults to downtimeDefaultLimit.
const (
	downtimeDefaultLimit = 50
	downtimeMaxLimit     = 500
)

// downtimeResponse is GET /api/downtime's JSON body: one page of the downtime events
// overlapping the requested range (newest-first, sliced by Limit/Offset), plus Total.
type downtimeResponse struct {
	Events       []DownEventView          `json:"events"`
	Total        int                      `json:"total"`
	TotalSeconds int64                    `json:"total_seconds"`
	Timeline     []DowntimeTimelineBucket `json:"timeline"`
	Limit        int                      `json:"limit"`
	Offset       int                      `json:"offset"`
}

// downtimeTimelineBuckets is the fixed number of equal-width buckets GET /api/downtime's
// Timeline field divides [from,to] into.
const downtimeTimelineBuckets = 120

// DowntimeTimelineBucket is one equal-width slice of GET /api/downtime's requested
// [from,to] range: Start/End are its Unix-second boundaries.
type DowntimeTimelineBucket struct {
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	DownSeconds int64  `json:"down_seconds"`
	Type        string `json:"type,omitempty"`
}

// buildDowntimeTimeline buckets evs (the caller passes every in-range event, never just one
// page) into downtimeTimelineBuckets equal-width buckets spanning exactly [from,to].
func buildDowntimeTimeline(evs []DownEventView, from, to int64) ([]DowntimeTimelineBucket, int64) {
	n := downtimeTimelineBuckets
	buckets := make([]DowntimeTimelineBucket, n)
	span := to - from
	for i := 0; i < n; i++ {
		buckets[i].Start = from + span*int64(i)/int64(n)
		buckets[i].End = from + span*int64(i+1)/int64(n)
	}

	var total int64
	for _, e := range evs {
		s, en := e.Start, e.End
		if en == 0 {
			en = to
		}
		if s < from {
			s = from
		}
		if en > to {
			en = to
		}
		if en <= s {
			continue
		}
		for i := range buckets {
			os, oe := s, en
			if buckets[i].Start > os {
				os = buckets[i].Start
			}
			if buckets[i].End < oe {
				oe = buckets[i].End
			}
			if oe <= os {
				continue
			}
			overlap := oe - os
			buckets[i].DownSeconds += overlap
			total += overlap
			if e.Type == "power_down" {
				buckets[i].Type = "power_down"
			} else if buckets[i].Type == "" {
				buckets[i].Type = e.Type
			}
		}
	}
	return buckets, total
}

// parseDowntimePagination validates GET /api/downtime's optional limit/ offset query
// parameters: limit defaults to downtimeDefaultLimit and must parse as an integer in [1.
func parseDowntimePagination(r *http.Request) (limit, offset int, ok bool) {
	q := r.URL.Query()

	limit = downtimeDefaultLimit
	if s := q.Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 || v > downtimeMaxLimit {
			return 0, 0, false
		}
		limit = v
	}

	offset = 0
	if s := q.Get("offset"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return 0, 0, false
		}
		offset = v
	}

	return limit, offset, true
}

// downtimeAPIHandler serves GET /api/downtime?from=&to=&limit=&offset=: the downtime-event
// feed the history page's "Downtime · 30d" panel fetches.
func downtimeAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := parseSeriesRange(r)
		if !ok {
			http.Error(w, "invalid from/to range", http.StatusBadRequest)
			return
		}
		limit, offset, ok := parseDowntimePagination(r)
		if !ok {
			http.Error(w, "invalid limit/offset", http.StatusBadRequest)
			return
		}

		resp := downtimeResponse{Events: []DownEventView{}, Limit: limit, Offset: offset}
		var sorted []DownEventView
		if api := apiFor(r, d); api != nil {
			evs, err := api.Events(from, to)
			if err != nil {
				log.Printf("web: /api/downtime query from=%d to=%d: %v", from, to, err)
			} else if len(evs) > 0 {
				sorted = make([]DownEventView, len(evs))
				copy(sorted, evs)
				sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })
			}
		}

		resp.Total = len(sorted)
		resp.Timeline, resp.TotalSeconds = buildDowntimeTimeline(sorted, from, to)

		if len(sorted) > 0 {
			start := offset
			if start > len(sorted) {
				start = len(sorted)
			}
			end := start + limit
			if end > len(sorted) {
				end = len(sorted)
			}
			resp.Events = sorted[start:end]
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// HistoryPageData is what templates/history.html renders against: the shared PageData plus
// the current filesystem mounts.
type HistoryPageData struct {
	PageData
	// DiskMounts is the sorted mount paths (DashboardView.Disks) the disk
	// panel graphs; empty when no filesystems are known (renders a note).
	DiskMounts []string
	// DiskMetrics is DiskMounts as the comma-joined "disk:<mount>" metric list history.html
	// hands the disk chart's data-metrics attribute.
	DiskMetrics string
	DiskLabels  string
}

// historyPageHandler renders GET /history (templates/history.html) through the full
// app-shell layout, same as dashboardHandler.
func historyPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mounts := historyDiskMounts(r, d)
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

// renderHistoryPage renders templates/history.html through the full app-shell layout
// (base.html) against HistoryPageData -- the same parse/execute shape renderDashboardPage.
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
func historyDiskMounts(r *http.Request, d Deps) []string {
	var view DashboardView
	if nodeFrom(r).Self {
		if d.Snapshot == nil {
			return nil
		}
		view = d.Snapshot()
	} else {
		api := apiFor(r, d)
		if api == nil {
			return nil
		}
		v, err := api.Snapshot()
		if err != nil {
			return nil
		}
		view = v
	}
	if len(view.Disks) == 0 {
		return nil
	}
	mounts := make([]string, 0, len(view.Disks))
	for _, disk := range view.Disks {
		mounts = append(mounts, disk.Mount)
	}
	return mounts
}
