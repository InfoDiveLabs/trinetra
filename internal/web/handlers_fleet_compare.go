package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetCompareMetric is /fleet/compare's metric selector (task-1b-brief.md:
// "Metrics: cpu, mem, swap, load1, temp, disk (worst mount)") -- a
// deliberately different set from the heatmap's fleetMetric (cpu/mem/disk/
// load), since the compare view exposes every metric FleetSeries can serve,
// not just the four the heatmap tiles show.
type fleetCompareMetric string

const (
	compareMetricCPU   fleetCompareMetric = "cpu"
	compareMetricMem   fleetCompareMetric = "mem"
	compareMetricSwap  fleetCompareMetric = "swap"
	compareMetricLoad1 fleetCompareMetric = "load1"
	compareMetricTemp  fleetCompareMetric = "temp"
	compareMetricDisk  fleetCompareMetric = "disk"
)

var fleetCompareMetricLabels = []struct {
	key   fleetCompareMetric
	label string
}{
	{compareMetricCPU, "CPU"},
	{compareMetricMem, "Memory"},
	{compareMetricSwap, "Swap"},
	{compareMetricLoad1, "Load (1m)"},
	{compareMetricTemp, "Temperature"},
	{compareMetricDisk, "Disk (worst)"},
}

// parseFleetCompareMetric reads ?metric= off r, defaulting to cpu for
// anything absent/unrecognized -- the same convention parseFleetMetric
// already uses for the heatmap's own selector.
func parseFleetCompareMetric(r *http.Request) fleetCompareMetric {
	got := fleetCompareMetric(r.URL.Query().Get("metric"))
	for _, m := range fleetCompareMetricLabels {
		if m.key == got {
			return got
		}
	}
	return compareMetricCPU
}

// fleetCompareRangeSeconds are /fleet/compare's four allowed ?range= values
// (task-1b-brief.md: "Range is one of 1h/6h/24h/7d"), in display order.
var fleetCompareRangeSeconds = []struct {
	key     string
	seconds int64
}{
	{"1h", 3600},
	{"6h", 21600},
	{"24h", 86400},
	{"7d", 604800},
}

// parseFleetCompareRange reads ?range= off r, defaulting to 6h for anything
// absent/unrecognized.
func parseFleetCompareRange(r *http.Request) string {
	got := r.URL.Query().Get("range")
	for _, rg := range fleetCompareRangeSeconds {
		if rg.key == got {
			return got
		}
	}
	return "6h"
}

func fleetCompareRangeWindow(rangeKey string) int64 {
	for _, rg := range fleetCompareRangeSeconds {
		if rg.key == rangeKey {
			return rg.seconds
		}
	}
	return 21600
}

// fleetCompareResolution picks raw vs 1m off the requested range
// (task-1b-brief.md: "Resolution: raw for ranges up to 6h, 1m above that").
func fleetCompareResolution(rangeKey string) core.Resolution {
	if rangeKey == "1h" || rangeKey == "6h" {
		return core.ResRaw
	}
	return core.Res1m
}

// fleetCompareColors is the data-series palette /fleet/compare's chart uses,
// one entry per node line, cycling for more than four nodes -- the exact
// hex values assets/app.js's CHART_COLORS already draws chart strokes with
// (pink/info/cyan/violet), so a node's line color here and its legend swatch
// there always agree. Ember (--crit/--signal) never appears here: this is a
// data series, not a "look here" signal (global-constraints.md's ember
// discipline).
var fleetCompareColors = []string{"#e87ba6", "#4aa3ff", "#34d399", "#a78bfa"}

// FleetCompareOption is one link in the metric or range selector.
type FleetCompareOption struct {
	Label  string
	Href   string
	Active bool
}

// FleetCompareLegendEntry is one line in the chart's legend: a compared
// node's display name and the color its series is drawn in.
type FleetCompareLegendEntry struct {
	Name  string
	Color string
}

// FleetComparePageData is what templates/fleet_compare.html's "content"
// block renders against.
type FleetComparePageData struct {
	PageData
	Metric        fleetCompareMetric
	MetricOptions []FleetCompareOption
	Range         string
	RangeOptions  []FleetCompareOption
	// Selected is true once the request named at least one node (?nodes=)
	// or a tag (?tag=) to compare; false means the page shows its initial
	// "nothing selected yet" prompt instead of a chart.
	Selected bool
	// ErrorMsg is a validation/cap error rendered inline (task-1b-brief.md:
	// "the cap error is shown inline"), e.g. FleetSeries' "compare at most
	// 10 nodes" -- never a 500.
	ErrorMsg string
	Legend   []FleetCompareLegendEntry
	// DataJSON is the chart's data, JSON-encoded and embedded as a data
	// attribute (assets/app.js's swBootFleetCompare reads and JSON.parses
	// it) rather than fetched separately, so the whole page -- including
	// its one FleetSeries call -- renders from a single request.
	DataJSON string
}

// fleetCompareData is DataJSON's shape: uPlot's own parallel-array data
// format (Series[0] the shared timestamp axis, Series[1:] one array per
// node in Nodes' order, aligned to that axis with null for a gap) -- the
// same convention handlers_history.go's seriesResponse uses for a single
// series, extended here to however many node series the request selected.
type fleetCompareData struct {
	Metric string        `json:"metric"`
	Nodes  []string      `json:"nodes"`
	Series []interface{} `json:"series"`
}

// parseFleetCompareFilter reads /fleet/compare's node selection off r:
// either an explicit ?nodes=a,b,c list (the /fleet table's "Compare" action)
// or a ?tag=web fallback -- the two forms task-1b-brief.md's URL examples
// give. Neither present means nothing is selected yet.
func parseFleetCompareFilter(r *http.Request) (filter core.NodeFilter, selected bool) {
	q := r.URL.Query()
	if raw := strings.TrimSpace(q.Get("nodes")); raw != "" {
		var ids []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				ids = append(ids, s)
			}
		}
		if len(ids) > 0 {
			return core.NodeFilter{Nodes: ids}, true
		}
	}
	if tag := strings.TrimSpace(q.Get("tag")); tag != "" {
		return core.NodeFilter{Tag: tag}, true
	}
	return core.NodeFilter{}, false
}

// fleetCompareOptionsQuery rebuilds r's query string with key set to value,
// used by the metric/range selector links so switching one never drops the
// other or the current node/tag selection.
func fleetCompareOptionsQuery(r *http.Request, key, value string) string {
	q := r.URL.Query()
	q.Set(key, value)
	return "/fleet/compare?" + q.Encode()
}

func fleetCompareMetricOptions(r *http.Request, active fleetCompareMetric) []FleetCompareOption {
	opts := make([]FleetCompareOption, 0, len(fleetCompareMetricLabels))
	for _, m := range fleetCompareMetricLabels {
		opts = append(opts, FleetCompareOption{Label: m.label, Href: fleetCompareOptionsQuery(r, "metric", string(m.key)), Active: m.key == active})
	}
	return opts
}

func fleetCompareRangeOptions(r *http.Request, active string) []FleetCompareOption {
	opts := make([]FleetCompareOption, 0, len(fleetCompareRangeSeconds))
	for _, rg := range fleetCompareRangeSeconds {
		opts = append(opts, FleetCompareOption{Label: rg.key, Href: fleetCompareOptionsQuery(r, "range", rg.key), Active: rg.key == active})
	}
	return opts
}

// buildFleetCompareChart groups pts (a flat FleetSeries agg="none" result)
// by node into uPlot's parallel-array shape, plus the legend entries the
// template renders alongside it. Node order is first-seen in pts, which is
// registry/self order (fleet_provider.go's FleetSeries) -- stable across a
// request, though not alphabetical.
func buildFleetCompareChart(metric string, pts []core.FleetSeriesPoint) (string, []FleetCompareLegendEntry, error) {
	var order []string
	byNode := map[string]map[int64]float64{}
	tsSet := map[int64]bool{}
	for _, p := range pts {
		if _, ok := byNode[p.Node]; !ok {
			order = append(order, p.Node)
			byNode[p.Node] = map[int64]float64{}
		}
		byNode[p.Node][p.TS] = p.Value
		tsSet[p.TS] = true
	}
	tsList := make([]int64, 0, len(tsSet))
	for ts := range tsSet {
		tsList = append(tsList, ts)
	}
	sort.Slice(tsList, func(i, j int) bool { return tsList[i] < tsList[j] })

	tsF := make([]float64, len(tsList))
	for i, ts := range tsList {
		tsF[i] = float64(ts)
	}
	series := make([]interface{}, 0, len(order)+1)
	series = append(series, tsF)

	legend := make([]FleetCompareLegendEntry, 0, len(order))
	for i, name := range order {
		vals := make([]*float64, len(tsList))
		for j, ts := range tsList {
			if v, ok := byNode[name][ts]; ok {
				v := v
				vals[j] = &v
			}
		}
		series = append(series, vals)
		legend = append(legend, FleetCompareLegendEntry{Name: name, Color: fleetCompareColors[i%len(fleetCompareColors)]})
	}

	b, err := json.Marshal(fleetCompareData{Metric: metric, Nodes: order, Series: series})
	if err != nil {
		return "", nil, err
	}
	return string(b), legend, nil
}

// fleetCompareHandler serves GET /fleet/compare (task C1b): a single uPlot
// overlay comparing metric across the nodes named by ?nodes=a,b,c or
// ?tag=web, over one of four fixed ranges. Viewer-gated (routes.go) and
// master-only (fleetGateHTML, exactly like every other /fleet* page) --
// solo/child both 404 here, never render an empty compare page.
func fleetCompareHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		metric := parseFleetCompareMetric(r)
		rangeKey := parseFleetCompareRange(r)
		filter, selected := parseFleetCompareFilter(r)

		data := FleetComparePageData{
			PageData:      newPageData(r, d, "Compare", "fleet metric comparison"),
			Metric:        metric,
			MetricOptions: fleetCompareMetricOptions(r, metric),
			Range:         rangeKey,
			RangeOptions:  fleetCompareRangeOptions(r, rangeKey),
			Selected:      selected,
		}

		if selected {
			var api core.FleetAPI
			if d.Fleet != nil {
				api = d.Fleet()
			}
			if api == nil {
				data.ErrorMsg = "fleet is not available"
			} else {
				to := time.Now().Unix()
				from := to - fleetCompareRangeWindow(rangeKey)
				pts, err := api.FleetSeries(string(metric), filter, core.AggNone, from, to, fleetCompareResolution(rangeKey))
				if err != nil {
					data.ErrorMsg = err.Error()
				} else {
					dataJSON, legend, err := buildFleetCompareChart(string(metric), pts)
					if err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					data.DataJSON, data.Legend = dataJSON, legend
				}
			}
		}

		if err := renderFleetComparePage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// renderFleetComparePage renders templates/fleet_compare.html's "content"
// block through the full app-shell layout, the same parse/execute shape
// renderFleetPage uses.
func renderFleetComparePage(w http.ResponseWriter, data FleetComparePageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_compare.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}
