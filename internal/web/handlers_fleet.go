package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetSkewWarnSec mirrors the CLI's clock-skew warning cutoff
// (internal/trinetra/fleet_cmd.go's skewWarnCLI = 30).
const fleetSkewWarnSec = 30

// fleetQuery is /fleet's (and /fleet/table's and /api/fleet/nodes') resolved query string:
// the three core.NodeFilter criteria plus the table's sort column/direction.
type fleetQuery struct {
	Tag, State, Query string
	Sort, Dir         string
}

// parseFleetQuery reads tag/state/q/sort/dir off r's query string. dir is
// normalized to exactly "asc" or "desc" (anything else, including absent,
// becomes "asc") so template/link-building code never has to special-case a
// third value.
func parseFleetQuery(r *http.Request) fleetQuery {
	q := r.URL.Query()
	dir := q.Get("dir")
	if dir != "desc" {
		dir = "asc"
	}
	return fleetQuery{
		Tag:   q.Get("tag"),
		State: q.Get("state"),
		Query: q.Get("q"),
		Sort:  q.Get("sort"),
		Dir:   dir,
	}
}

// encode renders fq back to a URL query string (tag/state/q included only when set;
// sort/dir included only when a sort column is chosen).
func (fq fleetQuery) encode() string {
	v := url.Values{}
	if fq.Tag != "" {
		v.Set("tag", fq.Tag)
	}
	if fq.State != "" {
		v.Set("state", fq.State)
	}
	if fq.Query != "" {
		v.Set("q", fq.Query)
	}
	if fq.Sort != "" {
		v.Set("sort", fq.Sort)
		v.Set("dir", fq.Dir)
	}
	return v.Encode()
}

// fleetStateMatches reports whether a node's State satisfies fq's state filter, with one
// deliberate, documented deviation from plain core.NodeFilter.Match semantics.
func fleetStateMatches(want, got string) bool {
	if want == "" {
		return true
	}
	if want == "lagging" {
		return got == "lagging" || got == "stale"
	}
	return want == got
}

// fleetHealth is the /fleet health strip's four counts, over the FULL (unfiltered) roster.
type fleetHealth struct {
	Online, Behind, Down, Revoked int
}

// computeFleetHealth buckets nodes by NodeSummary.State: online, lagging+stale combined as
// "Behind", down, revoked.
func computeFleetHealth(nodes []core.NodeSummary) fleetHealth {
	var h fleetHealth
	for _, n := range nodes {
		switch n.State {
		case "online":
			h.Online++
		case "lagging", "stale":
			h.Behind++
		case "down":
			h.Down++
		case "revoked":
			h.Revoked++
		}
	}
	return h
}

// FleetRow is one row of the /fleet table: NodeSummary plus the display-only fields the
// template needs and can't compute itself.
type FleetRow struct {
	core.NodeSummary
	// Href is this row's link target: "/" for self, "/n/<id>/" for a remote node, built via
	// nodeHref exactly like every other node-aware link in this package.
	Href string
	// SkewText is fmtSkew's exact rendering (internal/trinetra/fleet_cmd.go, mirrored here
	// since internal/web can't import that package): "-" for self, "0s" for zero skew.
	SkewText string
	// SkewWarn is the CLI's clock-skew warning sentence verbatim
	// ("clock differs from this master's by <±Ns> (fix NTP on that host)",
	// internal/trinetra/fleet_cmd.go's printNodeWarnings), non-empty only
	// when |SkewSec| > fleetSkewWarnSec and the node isn't self.
	SkewWarn string
	// DropsWarn is the CLI's replica-drops warning sentence ("replica drops: N out of order, N
	// over the series limit, N duplicates (harmless re-sends)", the wording).
	DropsWarn string
	// OutboxText is humanBytes(OutboxBytes), empty when nothing is queued (OutboxBytes<=0) so
	// the Link column doesn't render a bare "0 B" for every healthy node.
	OutboxText string
}

// fleetSkewText mirrors internal/trinetra/fleet_cmd.go's fmtSkew verbatim.
func fleetSkewText(n core.NodeSummary) string {
	switch {
	case n.Self:
		return "-"
	case n.SkewSec == 0:
		return "0s"
	}
	return fmt.Sprintf("%+ds", n.SkewSec)
}

// fleetSkewWarnText renders the CLI's clock-skew warning sentence (see FleetRow.SkewWarn's
// doc) for n, or "" when it doesn't apply.
func fleetSkewWarnText(n core.NodeSummary) string {
	if n.Self || (n.SkewSec <= fleetSkewWarnSec && n.SkewSec >= -fleetSkewWarnSec) {
		return ""
	}
	return fmt.Sprintf("clock differs from this master's by %s (fix NTP on that host)", fleetSkewText(n))
}

// fleetDropsWarnText renders the CLI's replica-drops warning sentence (see
// FleetRow.DropsWarn's doc) for n, or "" when it doesn't apply.
func fleetDropsWarnText(n core.NodeSummary) string {
	if n.Self || (n.DroppedOutOfOrder == 0 && n.DroppedCardinality == 0) {
		return ""
	}
	return fmt.Sprintf("replica drops: %d out of order, %d over the series limit, %d duplicates (harmless re-sends)",
		n.DroppedOutOfOrder, n.DroppedCardinality, n.DroppedDuplicate)
}

// newFleetRow builds one FleetRow from a roster entry.
func newFleetRow(n core.NodeSummary) FleetRow {
	prefix := ""
	if !n.Self {
		prefix = "/n/" + n.ID
	}
	outbox := ""
	if n.OutboxBytes > 0 {
		outbox = humanBytes(uint64(n.OutboxBytes))
	}
	return FleetRow{
		NodeSummary: n,
		Href:        nodeHref(prefix, "/"),
		SkewText:    fleetSkewText(n),
		SkewWarn:    fleetSkewWarnText(n),
		DropsWarn:   fleetDropsWarnText(n),
		OutboxText:  outbox,
	}
}

// fleetSortKeys are the table's sortable columns, in header order -- fleetSortLinks builds
// one link per key (a th's href), sortFleetNodes switches on the same set.
var fleetSortKeys = []string{"name", "state", "cpu", "mem", "disk", "load", "version", "lastseen"}

// fleetSortLinks builds, for every sortable column, the href that column's header link
// should carry: the current tag/state/q filter preserved, sort=<key>.
func fleetSortLinks(fq fleetQuery) map[string]string {
	links := make(map[string]string, len(fleetSortKeys))
	for _, key := range fleetSortKeys {
		dir := "asc"
		if fq.Sort == key && fq.Dir == "asc" {
			dir = "desc"
		}
		next := fleetQuery{Tag: fq.Tag, State: fq.State, Query: fq.Query, Sort: key, Dir: dir}
		links[key] = "/fleet?" + next.encode()
	}
	return links
}

func lessStr(a, b string, desc bool) bool {
	if desc {
		return a > b
	}
	return a < b
}

func lessFloat(a, b float64, desc bool) bool {
	if desc {
		return a > b
	}
	return a < b
}

func lessInt64(a, b int64, desc bool) bool {
	if desc {
		return a > b
	}
	return a < b
}

// sortFleetNodes sorts nodes in place by sortKey/dir.
func sortFleetNodes(nodes []core.NodeSummary, sortKey, dir string) {
	desc := dir == "desc"
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		switch sortKey {
		case "name":
			return lessStr(a.Name, b.Name, desc)
		case "state":
			return lessStr(a.State, b.State, desc)
		case "cpu":
			return lessFloat(a.CPU, b.CPU, desc)
		case "mem":
			return lessFloat(a.MemPct, b.MemPct, desc)
		case "disk":
			return lessFloat(a.WorstDiskPct, b.WorstDiskPct, desc)
		case "load":
			return lessFloat(a.Load1, b.Load1, desc)
		case "version":
			return lessStr(a.Version, b.Version, desc)
		case "lastseen":
			return lessInt64(a.LastSeen, b.LastSeen, desc)
		default:
			ad, bd := a.State == "down", b.State == "down"
			if ad != bd {
				return ad
			}
			return a.Name < b.Name
		}
	})
}

// fetchFleetNodes returns the full, unfiltered node roster via r's request-scoped fleetMemo
// (fleet_memo.go).
func fetchFleetNodes(r *http.Request, d Deps) []core.NodeSummary {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return nil
	}
	return nodes
}

// fleetNodeNameLookup builds a node-id -> display-name map off r's request- scoped roster
// (fetchFleetNodes), for a page that has only raw ids to show.
func fleetNodeNameLookup(r *http.Request, d Deps) map[string]string {
	nodes := fetchFleetNodes(r, d)
	names := make(map[string]string, len(nodes))
	for _, n := range nodes {
		names[n.ID] = n.Name
	}
	return names
}

// fleetMetric is the heatmap's metric selector (?metric=cpu|mem|disk|load),
// defaulting to cpu.
type fleetMetric string

const (
	fleetMetricCPU  fleetMetric = "cpu"
	fleetMetricMem  fleetMetric = "mem"
	fleetMetricDisk fleetMetric = "disk"
	fleetMetricLoad fleetMetric = "load"
)

// fleetMetricLabels is the metric selector's link order/labels, top to bottom -- also
// fleetMetricOptions' iteration order, so the chips render CPU, Memory, Disk.
var fleetMetricLabels = []struct {
	key   fleetMetric
	label string
}{
	{fleetMetricCPU, "CPU"},
	{fleetMetricMem, "Memory"},
	{fleetMetricDisk, "Disk"},
	{fleetMetricLoad, "Load"},
}

// parseFleetMetric reads ?metric= off r's query string, defaulting to cpu for anything
// absent/unrecognized.
func parseFleetMetric(r *http.Request) fleetMetric {
	switch fleetMetric(r.URL.Query().Get("metric")) {
	case fleetMetricMem:
		return fleetMetricMem
	case fleetMetricDisk:
		return fleetMetricDisk
	case fleetMetricLoad:
		return fleetMetricLoad
	default:
		return fleetMetricCPU
	}
}

// fleetPageQueryString returns fq's encoded query string (its own encode(),
// UNCHANGED -- see that method's doc; the table's htmx poll relies on its
// exact output, so this never mutates fq or its encode()) plus a trailing
// metric=. Used by the heatmap's metric-selector links and the panels'
// "Refresh" link, which reload the full page with the current filter/sort
// AND the current metric selection; the table's own poll deliberately does
// NOT carry metric (fleetQuery.encode() has no such field), since the
// selected heatmap metric has no bearing on the table's rows.
func fleetPageQueryString(fq fleetQuery, metric fleetMetric) string {
	v, _ := url.ParseQuery(fq.encode())
	v.Set("metric", string(metric))
	return v.Encode()
}

// FleetMetricOption is one entry in the heatmap's metric selector -- a plain
// link, not a <select>/radio group.
type FleetMetricOption struct {
	Label  string
	Href   string
	Active bool
}

// fleetMetricOptions builds the metric selector's four options in fleetMetricLabels' order,
// each link carrying fq's other query params.
func fleetMetricOptions(fq fleetQuery, active fleetMetric) []FleetMetricOption {
	opts := make([]FleetMetricOption, 0, len(fleetMetricLabels))
	for _, m := range fleetMetricLabels {
		opts = append(opts, FleetMetricOption{
			Label:  m.label,
			Href:   "/fleet?" + fleetPageQueryString(fq, m.key),
			Active: m.key == active,
		})
	}
	return opts
}

// fleetHeatBand is the heatmap tile's discrete colour band: below 70% is neutral
// (graphite/slate, .heat-neutral), 70-85% is amber at low intensity (.heat-amber-low).
type fleetHeatBand string

const (
	fleetHeatNeutral   fleetHeatBand = "neutral"
	fleetHeatAmberLow  fleetHeatBand = "amber-low"
	fleetHeatAmberFull fleetHeatBand = "amber-full"
	fleetHeatDown      fleetHeatBand = "down"
	fleetHeatRevoked   fleetHeatBand = "revoked"
)

// pctHeatBand buckets a 0-100 percentage metric (CPU/mem/disk) into the
// three numeric bands of fleetHeatBand.
func pctHeatBand(pct float64) fleetHeatBand {
	switch {
	case pct >= 85:
		return fleetHeatAmberFull
	case pct >= 70:
		return fleetHeatAmberLow
	default:
		return fleetHeatNeutral
	}
}

// Load1's absolute heatmap thresholds. core.NodeSummary carries no core-count field at all
// (see its doc in internal/core/fleet.go).
const (
	fleetLoadAmberLowAt  = 1.0
	fleetLoadAmberFullAt = 4.0
)

// loadHeatBand buckets an absolute Load1 value per the thresholds above.
func loadHeatBand(load float64) fleetHeatBand {
	switch {
	case load >= fleetLoadAmberFullAt:
		return fleetHeatAmberFull
	case load >= fleetLoadAmberLowAt:
		return fleetHeatAmberLow
	default:
		return fleetHeatNeutral
	}
}

// fleetHeatValue resolves metric's band and display text for n.
func fleetHeatValue(n core.NodeSummary, metric fleetMetric) (band fleetHeatBand, text string) {
	switch n.State {
	case "down":
		return fleetHeatDown, "down"
	case "revoked":
		return fleetHeatRevoked, "revoked"
	}
	switch metric {
	case fleetMetricMem:
		return pctHeatBand(n.MemPct), fmt.Sprintf("%.0f%%", n.MemPct)
	case fleetMetricDisk:
		return pctHeatBand(n.WorstDiskPct), fmt.Sprintf("%.0f%%", n.WorstDiskPct)
	case fleetMetricLoad:
		return loadHeatBand(n.Load1), fmt.Sprintf("%.2f", n.Load1)
	default: // fleetMetricCPU
		return pctHeatBand(n.CPU), fmt.Sprintf("%.0f%%", n.CPU)
	}
}

// FleetHeatTile is one heatmap tile: the roster entry plus the band/text/
// link the template can't compute itself.
type FleetHeatTile struct {
	core.NodeSummary
	Href string
	Band fleetHeatBand
	Text string
}

// buildFleetHeatTiles builds one FleetHeatTile per node in nodes (already filtered by the
// page's current tag/state/q, see fleetFilterNodes).
func buildFleetHeatTiles(nodes []core.NodeSummary, metric fleetMetric) []FleetHeatTile {
	tiles := make([]FleetHeatTile, 0, len(nodes))
	for _, n := range nodes {
		band, text := fleetHeatValue(n, metric)
		prefix := ""
		if !n.Self {
			prefix = "/n/" + n.ID
		}
		tiles = append(tiles, FleetHeatTile{NodeSummary: n, Href: nodeHref(prefix, "/"), Band: band, Text: text})
	}
	sort.SliceStable(tiles, func(i, j int) bool { return tiles[i].Name < tiles[j].Name })
	return tiles
}

// fleetTopN is the top-N panels' row count.
const fleetTopN = 5

// FleetTopRow is one row in a top-N ranking panel: reuses templates/dashboard.html's
// containerBar shape.
type FleetTopRow struct {
	Name      string
	Href      string
	ValueText string
	WidthPct  float64
}

// topNByMetric ranks nodes (excluding down/revoked) by value, descending, ties broken by
// name for a deterministic presentation order.
func topNByMetric(nodes []core.NodeSummary, value func(core.NodeSummary) float64) []FleetTopRow {
	elig := make([]core.NodeSummary, 0, len(nodes))
	for _, n := range nodes {
		if n.State == "down" || n.State == "revoked" {
			continue
		}
		elig = append(elig, n)
	}
	sort.SliceStable(elig, func(i, j int) bool {
		vi, vj := value(elig[i]), value(elig[j])
		if vi != vj {
			return vi > vj
		}
		return elig[i].Name < elig[j].Name
	})
	if len(elig) > fleetTopN {
		elig = elig[:fleetTopN]
	}
	rows := make([]FleetTopRow, 0, len(elig))
	for _, n := range elig {
		prefix := ""
		if !n.Self {
			prefix = "/n/" + n.ID
		}
		v := value(n)
		width := v
		if width > 100 {
			width = 100
		}
		if width < 0 {
			width = 0
		}
		rows = append(rows, FleetTopRow{Name: n.Name, Href: nodeHref(prefix, "/"), ValueText: fmt.Sprintf("%.0f%%", v), WidthPct: width})
	}
	return rows
}

// FleetDownRow is one row in the "Down now" panel: node name/link plus how
// long ago it was last seen (rendered via the "nodeAgo" template helper,
// same as the table's own Last seen column).
type FleetDownRow struct {
	Name     string
	Href     string
	LastSeen int64
}

// fleetDownRows lists every down node in nodes, name-ascending.
func fleetDownRows(nodes []core.NodeSummary) []FleetDownRow {
	rows := make([]FleetDownRow, 0)
	for _, n := range nodes {
		if n.State != "down" {
			continue
		}
		prefix := ""
		if !n.Self {
			prefix = "/n/" + n.ID
		}
		rows = append(rows, FleetDownRow{Name: n.Name, Href: nodeHref(prefix, "/"), LastSeen: n.LastSeen})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// fleetFilterNodes returns the subset of nodes passing fq's tag/state/q
// filters (fleetFilterMatch/fleetStateMatches -- see their docs, including
// fleetFilterMatch's non-admin RemoteAddr exclusion), in nodes' original
// order. Shared by the table (buildFleetRows sorts this further) and the
// heatmap/top-N panels, which respect the same filters.
func fleetFilterNodes(nodes []core.NodeSummary, fq fleetQuery, admin bool) []core.NodeSummary {
	tagQuery := core.NodeFilter{Tag: fq.Tag, Query: fq.Query}
	filtered := make([]core.NodeSummary, 0, len(nodes))
	for _, n := range nodes {
		if !fleetFilterMatch(n, tagQuery, admin) {
			continue
		}
		if !fleetStateMatches(fq.State, n.State) {
			continue
		}
		filtered = append(filtered, n)
	}
	return filtered
}

// fleetFilterMatch reports whether n passes every one of f's criteria
// (State/Tag/Query -- the caller, e.g. buildFleetRows, may additionally
// apply its own State handling on top when it wants something other than
// f.State's exact match, such as fleetStateMatches' "lagging also matches
// stale" convenience; f.State is left "" in that case so this function's own
// State check is a no-op there), with one deliberate difference from plain
// core.NodeFilter.Match for a non-admin caller.
//
// core.NodeFilter.Match's Query haystack is "name + id + RemoteAddr", which
// would let a viewer probe a node's network address by searching for address
// fragments, even though RemoteAddr is otherwise redacted from a viewer's view
// entirely (fleetNodesAPIHandler's own RemoteAddr-blanking, and the HTML table
// never rendering it at all). admin==true keeps Match's exact semantics -- an
// admin session already sees the real RemoteAddr elsewhere, so matching
// against it here leaks nothing new. admin==false instead matches Query
// against name+id+tags, never RemoteAddr; State and Tag are identical either
// way.
//
// This is a web-layer helper, not a change to core.NodeFilter.Match itself, so
// the non-admin path duplicates Match's State/Tag checks verbatim rather than
// calling into it.
func fleetFilterMatch(n core.NodeSummary, f core.NodeFilter, admin bool) bool {
	if admin {
		return f.Match(n)
	}
	if f.State != "" && n.State != f.State {
		return false
	}
	if f.Tag != "" && !slices.Contains(n.Tags, f.Tag) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		hay := strings.ToLower(n.Name + " " + n.ID + " " + strings.Join(n.Tags, " "))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// buildFleetRows filters nodes by fq (tag/query via fleetFilterMatch, gated
// by admin so a non-admin caller's q= never matches RemoteAddr -- see that
// func's doc; state via fleetStateMatches, see its doc for the one
// documented deviation), sorts the result, and projects each survivor into
// a FleetRow. Shared by the full page (buildFleetPageData) and the bare
// htmx fragment (fleetTableHandler) so both render identically.
func buildFleetRows(nodes []core.NodeSummary, fq fleetQuery, admin bool) []FleetRow {
	return sortAndBuildFleetRows(fleetFilterNodes(nodes, fq, admin), fq)
}

// sortAndBuildFleetRows sorts an ALREADY-filtered node slice (fq.Sort/fq.Dir, in place) and
// projects it into FleetRows.
func sortAndBuildFleetRows(filtered []core.NodeSummary, fq fleetQuery) []FleetRow {
	sortFleetNodes(filtered, fq.Sort, fq.Dir)
	rows := make([]FleetRow, 0, len(filtered))
	for _, n := range filtered {
		rows = append(rows, newFleetRow(n))
	}
	return rows
}

// FleetTableData is what templates/fleet.html's "fleet_rows" block (the bare <tbody>
// fragment /fleet/table returns) renders against.
type FleetTableData struct {
	Rows        []FleetRow
	QueryString string
}

// FleetPageData is what templates/fleet.html's "content" block (the full
// /fleet page) renders against.
type FleetPageData struct {
	PageData
	Rows        []FleetRow
	Health      fleetHealth
	Query       fleetQuery
	QueryString string
	SortLinks   map[string]string
	TotalNodes  int
	// Metric/MetricOptions/RefreshHref/HeatTiles/TopCPU/TopMem/TopDisk/DownNow back the
	// heatmap and top-N panels that sit above the table -- see buildFleetPageData.
	Metric        fleetMetric
	MetricOptions []FleetMetricOption
	RefreshHref   string
	HeatTiles     []FleetHeatTile
	TopCPU        []FleetTopRow
	TopMem        []FleetTopRow
	TopDisk       []FleetTopRow
	DownNow       []FleetDownRow
}

// buildFleetPageData assembles FleetPageData for GET /fleet: the full roster (for the
// health strip), the current query's filtered/sorted rows.
func buildFleetPageData(r *http.Request, d Deps) FleetPageData {
	fq := parseFleetQuery(r)
	metric := parseFleetMetric(r)
	admin := currentRole(r) == "admin"
	nodes := fetchFleetNodes(r, d)
	filtered := fleetFilterNodes(nodes, fq, admin)
	sub := fmt.Sprintf("%d node", len(nodes))
	if len(nodes) != 1 {
		sub += "s"
	}

	// Every one of these reads filtered without depending on its order (each does its own
	// sort/selection internally).
	heatTiles := buildFleetHeatTiles(filtered, metric)
	topCPU := topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.CPU })
	topMem := topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.MemPct })
	topDisk := topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.WorstDiskPct })
	downNow := fleetDownRows(filtered)
	rows := sortAndBuildFleetRows(filtered, fq)

	return FleetPageData{
		PageData:      newPageData(r, d, "Fleet", sub),
		Rows:          rows,
		Health:        computeFleetHealth(nodes),
		Query:         fq,
		QueryString:   fq.encode(),
		SortLinks:     fleetSortLinks(fq),
		TotalNodes:    len(nodes),
		Metric:        metric,
		MetricOptions: fleetMetricOptions(fq, metric),
		RefreshHref:   "/fleet?" + fleetPageQueryString(fq, metric),
		HeatTiles:     heatTiles,
		TopCPU:        topCPU,
		TopMem:        topMem,
		TopDisk:       topDisk,
		DownNow:       downNow,
	}
}

// renderFleetPage renders templates/fleet.html's "content" block through the full app-shell
// layout (base.html).
func renderFleetPage(w http.ResponseWriter, data FleetPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderFleetTableFragment renders templates/fleet.html's "fleet_rows" block alone -- no
// base.html, no "content" wrapper.
func renderFleetTableFragment(w http.ResponseWriter, data FleetTableData) error {
	tmpl, err := template.New("fleet.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/fleet.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "fleet_rows", data)
}

// fleetGateOrNotFound is the shared "masters only" gate every /fleet*
// handler applies first (404 unless fleetRole(d)=='master'), reported via
// renderNotFound (styled page, for the HTML page) or a bare http.NotFound (for
// the fragment/JSON endpoints, which have no shell to render). It returns
// whether the caller should stop (true == already handled, a 404 was written).
// fleetRole(r, d) reads r's request-scoped fleetMemo (fleet_memo.go), so this
// gate's own Status() check costs a real round trip only once per request even
// though GET /fleet's page render AND (for the HTML path) newPageData's own
// resolveFleetPageInfo ask fleetRole/Status() questions too.
func fleetGateHTML(w http.ResponseWriter, r *http.Request, d Deps) bool {
	if fleetRole(r, d) == config.RoleMaster {
		return false
	}
	renderNotFound(w, r, d, "not found")
	return true
}

func fleetGatePlain(w http.ResponseWriter, r *http.Request, d Deps) bool {
	if fleetRole(r, d) == config.RoleMaster {
		return false
	}
	http.NotFound(w, r)
	return true
}

// fleetOverviewHandler serves GET /fleet: the health strip, filter form, and sortable node
// table.
func fleetOverviewHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		if err := renderFleetPage(w, buildFleetPageData(r, d)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetTableHandler serves GET /fleet/table: the htmx poll target.
func fleetTableHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGatePlain(w, r, d) {
			return
		}
		fq := parseFleetQuery(r)
		data := FleetTableData{Rows: buildFleetRows(fetchFleetNodes(r, d), fq, currentRole(r) == "admin"), QueryString: fq.encode()}
		if err := renderFleetTableFragment(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetNodesAPIHandler serves GET /api/fleet/nodes: the JSON roster, filtered by
// tag/state/q exactly as core.NodeFilter.Match defines it for an ADMIN caller.
func fleetNodesAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGatePlain(w, r, d) {
			return
		}
		q := r.URL.Query()
		filter := core.NodeFilter{Tag: q.Get("tag"), State: q.Get("state"), Query: q.Get("q")}
		nodes := fetchFleetNodes(r, d)
		admin := currentRole(r) == "admin"
		out := make([]core.NodeSummary, 0, len(nodes))
		for _, n := range nodes {
			if !fleetFilterMatch(n, filter, admin) {
				continue
			}
			if !admin {
				n.RemoteAddr = ""
			}
			out = append(out, n)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(out); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// --------------------------------------------------------------------------- Incidents:
// GET /fleet/incidents.

// fleetIncidentsPageSize is the incidents list's fixed page size
// (global-constraints.md: "Lists are paginated (50 per page)").
const fleetIncidentsPageSize = 50

// fleetIncidentsFiringCap bounds the nav badge's own Incidents() call (fleet_memo.go's
// fleetIncidentsFiringCount): the badge renders on EVERY page a fleet master serves.
const fleetIncidentsFiringCap = 1000

// incidentQuery is /fleet/incidents' resolved query string: the three core.IncidentFilter
// criteria this page exposes plus the current page number.
type incidentQuery struct {
	State, Node, Tag string
	Page             int
}

// parseIncidentQuery reads state/node/tag/page off r's query string, mirroring
// parseFleetQuery's convention.
func parseIncidentQuery(r *http.Request) incidentQuery {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	return incidentQuery{State: q.Get("state"), Node: q.Get("node"), Tag: q.Get("tag"), Page: page}
}

// encode renders iq back to a URL query string (state/node/tag included only when set, page
// only when > 1).
func (iq incidentQuery) encode() string {
	v := url.Values{}
	if iq.State != "" {
		v.Set("state", iq.State)
	}
	if iq.Node != "" {
		v.Set("node", iq.Node)
	}
	if iq.Tag != "" {
		v.Set("tag", iq.Tag)
	}
	if iq.Page > 1 {
		v.Set("page", strconv.Itoa(iq.Page))
	}
	return v.Encode()
}

// withPage returns a copy of iq with Page replaced -- used to build the
// pager's Prev/Next hrefs without mutating the page's own query.
func (iq incidentQuery) withPage(page int) incidentQuery {
	next := iq
	next.Page = page
	return next
}

// incidentDurationText renders a duration given in seconds using the same
// short-unit convention nodeDurText uses for a timestamp-since-now (seconds
// below a minute, minutes/hours/days above) -- internal/web can't import the
// CLI's own duration helpers (see nodeDurText's doc for the same layering
// reason), so this is an independent, tiny formatter taking a duration
// directly rather than a timestamp.
func incidentDurationText(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec < 3600:
		return fmt.Sprintf("%dm", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%dh", sec/3600)
	default:
		return fmt.Sprintf("%dd", sec/86400)
	}
}

// incidentTimeText renders a Unix timestamp through silenceTimeText
// (handlers_fleet_silences.go) -- the master's own local zone with its abbreviation.
func incidentTimeText(ts int64) string {
	return silenceTimeText(ts)
}

// IncidentRow is one row of the /fleet/incidents list table.
type IncidentRow struct {
	ID              string
	Href            string
	State           string
	Title           string
	Severity        string
	Nodes           string
	OpenedText      string
	DurationText    string
	DeliveredCount  int
	SuppressedCount int
}

// incidentNodeNames returns the distinct display names (IncidentAlert.
func incidentNodeNames(inc core.Incident) []string {
	seen := map[string]bool{}
	var names []string
	for _, a := range inc.Alerts {
		name := a.NodeName
		if name == "" {
			name = a.Node
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// nameSelfMembers gives every member alert the master raised itself (the engine submits
// those with an empty Node and NodeName) the master's own display name, so the list.
func nameSelfMembers(inc core.Incident, selfName string) core.Incident {
	if selfName == "" {
		return inc
	}
	alerts := make([]core.IncidentAlert, len(inc.Alerts))
	copy(alerts, inc.Alerts)
	for i := range alerts {
		if alerts[i].Node == "" && alerts[i].NodeName == "" {
			alerts[i].NodeName = selfName
		}
	}
	inc.Alerts = alerts
	return inc
}

// masterSelfName is this master's own display name (its NodeSummary with Self set), read
// through the request memo; "" when the roster is unavailable.
func masterSelfName(r *http.Request, d Deps) string {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return ""
	}
	for _, n := range nodes {
		if n.Self {
			return n.Name
		}
	}
	return ""
}

// incidentMemberKey identifies one incident member (a specific alert on a specific node)
// the same way fleet_incidents.go's own memberKey does.
func incidentMemberKey(node, key string) string {
	return node + "\x00" + key
}

// newIncidentRow projects one core.Incident into its list-row shape.
func newIncidentRow(inc core.Incident) IncidentRow {
	deliveredEv := map[string]bool{}
	suppressedEv := map[string]bool{}
	for _, ev := range inc.Timeline {
		switch ev.Kind {
		case "delivered":
			deliveredEv[incidentMemberKey(ev.Node, ev.AlertKey)] = true
		case "suppressed":
			suppressedEv[incidentMemberKey(ev.Node, ev.AlertKey)] = true
		}
	}
	delivered, suppressed := 0, 0
	for _, a := range inc.Alerts {
		mk := incidentMemberKey(a.Node, a.Key)
		if a.DeliveredLocally || deliveredEv[mk] {
			delivered++
		}
		if a.Suppressed != "" || a.SilencedBy != "" || suppressedEv[mk] {
			suppressed++
		}
	}
	end := time.Now().Unix()
	if inc.Resolved > 0 {
		end = inc.Resolved
	}
	dur := end - inc.Opened
	if dur < 0 {
		dur = 0
	}
	return IncidentRow{
		ID:              inc.ID,
		Href:            "/fleet/incidents/" + inc.ID,
		State:           inc.State,
		Title:           inc.Title,
		Severity:        inc.Severity,
		Nodes:           strings.Join(incidentNodeNames(inc), ", "),
		OpenedText:      incidentTimeText(inc.Opened),
		DurationText:    incidentDurationText(dur),
		DeliveredCount:  delivered,
		SuppressedCount: suppressed,
	}
}

// fetchIncidentsFiltered reads every incident matching iq's state/node/tag
// criteria straight from Fleet().Incidents (Limit 0 -- unlimited -- since
// pagination happens in memory, paginateIncidents below). Deliberately NOT
// read through the request-scoped fleetMemo (fleet_memo.go): the memo
// exists to protect a value several DIFFERENT call sites in one request
// might all ask for (the roster, fleet status); this specific, page's-own
// filtered query has exactly one caller per request, so memoizing it would
// just be a single-use cache. The nav badge's own, differently-filtered
// (state=firing, capped) query is what fleetIncidentsFiringCount memoizes
// instead. A nil Deps.Fleet, a nil FleetAPI, or a read error all degrade to
// an empty list rather than failing the page.
func fetchIncidentsFiltered(d Deps, iq incidentQuery) []core.Incident {
	fleet, err := fleetAPIFor(d)
	if err != nil {
		return nil
	}
	incs, err := fleet.Incidents(core.IncidentFilter{State: iq.State, Node: iq.Node, Tag: iq.Tag})
	if err != nil {
		return nil
	}
	return incs
}

// paginateIncidents slices all (already filtered, newest-updated-first per
// core.FleetAPI.Incidents' own contract) into page's 50-row window, clamping page into [1.
func paginateIncidents(all []core.Incident, page int) (pageItems []core.Incident, totalPages, clampedPage int) {
	total := len(all)
	totalPages = (total + fleetIncidentsPageSize - 1) / fleetIncidentsPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * fleetIncidentsPageSize
	if start > total {
		start = total
	}
	end := start + fleetIncidentsPageSize
	if end > total {
		end = total
	}
	return all[start:end], totalPages, page
}

// buildIncidentRows filters+paginates+projects all for either the full page or the bare
// fragment -- shared so both always render identically for the same query.
func buildIncidentRows(all []core.Incident, iq incidentQuery) (rows []IncidentRow, totalPages, clampedPage int) {
	page, totalPages, clampedPage := paginateIncidents(all, iq.Page)
	rows = make([]IncidentRow, 0, len(page))
	for _, inc := range page {
		rows = append(rows, newIncidentRow(inc))
	}
	return rows, totalPages, clampedPage
}

// IncidentsTableData is what templates/fleet_incidents.html's "incident_rows" block (the
// bare <tbody> fragment GET /fleet/incidents/table returns) renders against.
type IncidentsTableData struct {
	Rows        []IncidentRow
	QueryString string
}

// IncidentsPageData is what templates/fleet_incidents.html's "content" block
// renders against.
type IncidentsPageData struct {
	PageData
	Rows        []IncidentRow
	Query       incidentQuery
	QueryString string
	Total       int
	Page        int
	TotalPages  int
	HasPrev     bool
	HasNext     bool
	PrevHref    string
	NextHref    string
}

// incidentPageHref builds the pager's Prev/Next href: iq's other
// state/node/tag criteria preserved, page replaced.
func incidentPageHref(iq incidentQuery, page int) string {
	return "/fleet/incidents?" + iq.withPage(page).encode()
}

func buildIncidentsPageData(r *http.Request, d Deps) IncidentsPageData {
	iq := parseIncidentQuery(r)
	all := fetchIncidentsFiltered(d, iq)
	self := masterSelfName(r, d)
	for i := range all {
		all[i] = nameSelfMembers(all[i], self)
	}
	rows, totalPages, page := buildIncidentRows(all, iq)
	iq.Page = page
	sub := fmt.Sprintf("%d incident", len(all))
	if len(all) != 1 {
		sub += "s"
	}
	return IncidentsPageData{
		PageData:    newPageData(r, d, "Incidents", sub),
		Rows:        rows,
		Query:       iq,
		QueryString: iq.encode(),
		Total:       len(all),
		Page:        page,
		TotalPages:  totalPages,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
		PrevHref:    incidentPageHref(iq, page-1),
		NextHref:    incidentPageHref(iq, page+1),
	}
}

func buildIncidentsTableData(r *http.Request, d Deps) IncidentsTableData {
	iq := parseIncidentQuery(r)
	all := fetchIncidentsFiltered(d, iq)
	self := masterSelfName(r, d)
	for i := range all {
		all[i] = nameSelfMembers(all[i], self)
	}
	rows, _, page := buildIncidentRows(all, iq)
	iq.Page = page
	return IncidentsTableData{Rows: rows, QueryString: iq.encode()}
}

// renderIncidentsPage renders templates/fleet_incidents.html's "content" block through the
// full app-shell layout, the same parse/execute shape renderFleetPage uses.
func renderIncidentsPage(w http.ResponseWriter, data IncidentsPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_incidents.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderIncidentsTableFragment renders templates/fleet_incidents.html's "incident_rows"
// block alone -- no base.html, no "content" wrapper.
func renderIncidentsTableFragment(w http.ResponseWriter, data IncidentsTableData) error {
	tmpl, err := template.New("fleet_incidents.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/fleet_incidents.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "incident_rows", data)
}

// fleetIncidentsHandler serves GET /fleet/incidents: the filterable, paginated incident
// list.
func fleetIncidentsHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		if err := renderIncidentsPage(w, buildIncidentsPageData(r, d)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetIncidentsTableHandler serves GET /fleet/incidents/table: the htmx poll target that
// keeps the list's rows fresh without reloading the page shell.
func fleetIncidentsTableHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGatePlain(w, r, d) {
			return
		}
		if err := renderIncidentsTableFragment(w, buildIncidentsTableData(r, d)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// IncidentTimelineRow is one line of an incident's (or one member alert's own Explain)
// pipeline trail, rendered from IncidentEvent's STRUCTURED fields.
type IncidentTimelineRow struct {
	When  string
	Kind  string
	Text  string
	Actor string
}

// incidentChannelsText renders a delivery's channel list for display: "all
// channels" for the literal wildcard ("*", meaning every enabled channel --
// the same convention fleetAlertingChannelOptions/*'s "* (every enabled
// channel)" checkbox uses), the channels themselves otherwise.
func incidentChannelsText(channels []string) string {
	if len(channels) == 1 && channels[0] == "*" {
		return "all channels"
	}
	return strings.Join(channels, ", ")
}

// incidentEventLegPrefix is the "<leg>: " prefix suppressedDetail (fleet_incidents.go) puts
// on a master-own alert's suppressed reason.
func incidentEventLegPrefix(leg, detail string) string {
	prefix := leg + ": "
	if strings.HasPrefix(detail, prefix) {
		return detail[len(prefix):]
	}
	return detail
}

// incidentEventText renders ev into one human-readable line: "<leg> · <node NAME> · <key>
// · policy <p>, step <n> → <channels>" (U1, 2026-09-25 UI audit fix).
func incidentEventText(ev core.IncidentEvent, nodeNames map[string]string) string {
	if ev.Leg == "" && ev.Policy == "" {
		if ev.Detail != "" {
			return ev.Detail
		}
		return ev.Kind
	}
	var parts []string
	parts = append(parts, ev.Leg)
	if ev.Node != "" || nodeNames[""] != "" {
		name := nodeNames[ev.Node]
		if name == "" {
			name = ev.Node
		}
		parts = append(parts, name)
	}
	if ev.AlertKey != "" {
		parts = append(parts, ev.AlertKey)
	}
	switch {
	case ev.Policy != "":
		step := fmt.Sprintf("policy %s, step %d", ev.Policy, ev.Step+1) // 1-based, like the policy editor
		if len(ev.Channels) > 0 {
			step += " → " + incidentChannelsText(ev.Channels)
		}
		parts = append(parts, step)
	case len(ev.Channels) > 0:
		parts = append(parts, incidentChannelsText(ev.Channels))
	case ev.Kind == "fired" || ev.Kind == "resolved":
		// fireDetail/recoverDetail's ENTIRE content ("fired on X"/"recovered
		// on X", or bare "fired"/"recovered" for a master-own alert) just
		// restates the leg+node already rendered above -- nothing left to add.
	case ev.Detail != "":
		parts = append(parts, incidentEventLegPrefix(ev.Leg, ev.Detail))
	}
	return strings.Join(parts, " · ")
}

// incidentNodeNameLookup builds a node-id -> display-name map from inc.Alerts (an id with
// no NodeName is simply omitted, so callers keep falling back to the raw id).
func incidentNodeNameLookup(inc core.Incident) map[string]string {
	names := make(map[string]string, len(inc.Alerts))
	for _, a := range inc.Alerts {
		if a.NodeName == "" {
			continue
		}
		names[a.Node] = a.NodeName // "" is the master itself, see nameSelfMembers
	}
	return names
}

// buildIncidentTimeline projects events (already chronological, per
// core.FleetAPI.Incident/Explain's own contract) into their display rows.
func buildIncidentTimeline(events []core.IncidentEvent, nodeNames map[string]string) []IncidentTimelineRow {
	rows := make([]IncidentTimelineRow, 0, len(events))
	for _, ev := range events {
		rows = append(rows, IncidentTimelineRow{
			When:  incidentTimeText(ev.TS),
			Kind:  ev.Kind,
			Text:  incidentEventText(ev, nodeNames),
			Actor: ev.Actor,
		})
	}
	return rows
}

// IncidentMemberRow is one row of the detail page's member-alerts table, plus
// that member's own Explain(key) trail for the small per-alert "Explain" panel.
type IncidentMemberRow struct {
	Node             string
	Key              string
	FiredText        string
	ResolvedText     string
	DeliveredLocally bool
	SilencedBy       string
	Folded           string
	// Open reports whether this member is still active (ResolvedAt==0) -- exactly the set
	// openMemberMatchers silences, and what CanSilence.
	Open    bool
	Explain []IncidentTimelineRow
}

// buildIncidentMembers projects inc.Alerts into their row shape.
// explainByKey carries each distinct alert key's own Explain(key) trail
// (fetched once per key by the caller, buildIncidentDetailPageData) for that
// member's "Explain" panel.
func buildIncidentMembers(inc core.Incident, explainByKey map[string][]core.IncidentEvent) []IncidentMemberRow {
	nodeNames := incidentNodeNameLookup(inc)
	rows := make([]IncidentMemberRow, 0, len(inc.Alerts))
	for _, a := range inc.Alerts {
		node := a.NodeName
		if node == "" {
			node = a.Node
		}
		silencedBy := a.SilencedBy
		if silencedBy == "" {
			silencedBy = "-"
		}
		folded := a.Suppressed
		if folded == "" {
			folded = "-"
		}
		rows = append(rows, IncidentMemberRow{
			Node:             node,
			Key:              a.Key,
			FiredText:        incidentTimeText(a.FiredAt),
			ResolvedText:     incidentTimeText(a.ResolvedAt),
			DeliveredLocally: a.DeliveredLocally,
			SilencedBy:       silencedBy,
			Folded:           folded,
			Open:             a.ResolvedAt == 0,
			Explain:          buildIncidentTimeline(explainByKey[a.Key], nodeNames),
		})
	}
	return rows
}

// openMemberMatchers builds the silence-from-incident form's matchers from the incident's
// OPEN members: node id plus rule (AlertKey), one matcher per member (the list is ORed).
func openMemberMatchers(inc core.Incident) []core.Matcher {
	seen := map[string]bool{}
	var out []core.Matcher
	for _, a := range inc.Alerts {
		if a.ResolvedAt != 0 {
			continue
		}
		dedupKey := a.Node + "\x00" + a.Key
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		out = append(out, core.Matcher{Node: a.Node, Rule: a.Key})
	}
	return out
}

// incidentSilenceDuration is one of the silence form's fixed duration choices,
// rendered into the <select>.
type incidentSilenceDuration struct {
	Key      string
	Label    string
	Selected bool
}

// incidentSilenceDurationChoices are the four choices, in display order.
var incidentSilenceDurationChoices = []struct {
	key     string
	label   string
	seconds int64
}{
	{"30m", "30 minutes", 1800},
	{"1h", "1 hour", 3600},
	{"4h", "4 hours", 14400},
	{"24h", "24 hours", 86400},
}

// incidentSilenceDurationSeconds resolves a posted "for" value to its duration in seconds;
// ok is false for anything outside the four fixed choices.
func incidentSilenceDurationSeconds(key string) (int64, bool) {
	for _, c := range incidentSilenceDurationChoices {
		if c.key == key {
			return c.seconds, true
		}
	}
	return 0, false
}

// incidentSilenceDurationOptions renders the fixed choices for the <select>,
// marking selected as the currently-chosen one (defaulting to the first
// choice, 30m, when selected is empty/unrecognized).
func incidentSilenceDurationOptions(selected string) []incidentSilenceDuration {
	if _, ok := incidentSilenceDurationSeconds(selected); !ok {
		selected = incidentSilenceDurationChoices[0].key
	}
	opts := make([]incidentSilenceDuration, 0, len(incidentSilenceDurationChoices))
	for _, c := range incidentSilenceDurationChoices {
		opts = append(opts, incidentSilenceDuration{Key: c.key, Label: c.label, Selected: c.key == selected})
	}
	return opts
}

// incidentDetailOptions is buildIncidentDetailPageData's input: the page's
// transient, this-response-only state -- a flash message (success, via the
// ack/silence handlers' redirect, or a validation/FleetAPI error, rendered
// in place) and the silence form's sticky "for"/comment input on a
// validation failure -- exactly like fleetAdminOptions backs
// buildFleetAdminPageData.
type incidentDetailOptions struct {
	Flash        string
	FlashErr     bool
	SilenceErr   string
	ForValue     string
	CommentValue string
}

// IncidentDetailPageData is what templates/fleet_incident.html's "content"
// block renders against.
type IncidentDetailPageData struct {
	PageData
	Incident         core.Incident
	OpenedText       string
	ResolvedText     string
	DurationText     string
	Members          []IncidentMemberRow
	Timeline         []IncidentTimelineRow
	CanAck           bool
	CanSilence       bool
	SilenceDurations []incidentSilenceDuration
	Flash            string
	FlashErr         bool
	SilenceErr       string
	CommentValue     string
}

// buildIncidentDetailPageData assembles IncidentDetailPageData for GET
// /fleet/incidents/{id} and every ack/silence mutation's re-render on a validation failure.
func buildIncidentDetailPageData(r *http.Request, d Deps, id string, opts incidentDetailOptions) (IncidentDetailPageData, error) {
	fleet, err := fleetAPIFor(d)
	if err != nil {
		return IncidentDetailPageData{}, err
	}
	inc, err := fleet.Incident(id)
	if err != nil {
		return IncidentDetailPageData{}, err
	}

	inc = nameSelfMembers(inc, masterSelfName(r, d))

	explainByKey := map[string][]core.IncidentEvent{}
	seenKeys := map[string]bool{}
	for _, a := range inc.Alerts {
		if a.Key == "" || seenKeys[a.Key] {
			continue
		}
		seenKeys[a.Key] = true
		if evs, eerr := fleet.Explain(a.Key); eerr == nil {
			explainByKey[a.Key] = evs
		}
	}

	members := buildIncidentMembers(inc, explainByKey)
	openCount := 0
	for _, m := range members {
		if m.Open {
			openCount++
		}
	}

	end := time.Now().Unix()
	if inc.Resolved > 0 {
		end = inc.Resolved
	}
	dur := end - inc.Opened
	if dur < 0 {
		dur = 0
	}

	return IncidentDetailPageData{
		PageData:         newPageData(r, d, "Incident", inc.Title),
		Incident:         inc,
		OpenedText:       incidentTimeText(inc.Opened),
		ResolvedText:     incidentTimeText(inc.Resolved),
		DurationText:     incidentDurationText(dur),
		Members:          members,
		Timeline:         buildIncidentTimeline(inc.Timeline, incidentNodeNameLookup(inc)),
		CanAck:           inc.State != "resolved",
		CanSilence:       inc.State != "resolved" && openCount > 0,
		SilenceDurations: incidentSilenceDurationOptions(opts.ForValue),
		Flash:            opts.Flash,
		FlashErr:         opts.FlashErr,
		SilenceErr:       opts.SilenceErr,
		CommentValue:     opts.CommentValue,
	}, nil
}

// renderIncidentDetailPage renders templates/fleet_incident.html's "content"
// block through the full app-shell layout, at the given status (200 for a
// normal render, a 4xx for a validation/FleetAPI error re-render -- see
// renderIncidentMutationError).
func renderIncidentDetailPage(w http.ResponseWriter, data IncidentDetailPageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_incident.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderIncidentNotFound renders the shared 404 panel for an unresolvable incident id --
// GET, ack, and silence all reach here identically when Fleet().Incident(id) itself errors.
func renderIncidentNotFound(w http.ResponseWriter, r *http.Request, d Deps) {
	renderNotFound(w, r, d, "no such incident")
}

// renderIncidentMutationError re-renders the incident detail page with a flash/sticky-form
// error at the given 4xx status: FleetAPI errors render as a flash message, never a 500.
func renderIncidentMutationError(w http.ResponseWriter, r *http.Request, d Deps, id string, opts incidentDetailOptions, status int) {
	data, err := buildIncidentDetailPageData(r, d, id, opts)
	if err != nil {
		renderIncidentNotFound(w, r, d)
		return
	}
	if rerr := renderIncidentDetailPage(w, data, status); rerr != nil {
		http.Error(w, rerr.Error(), http.StatusInternalServerError)
	}
}

// resolveIncidentFlash resolves GET /fleet/incidents/{id}'s ?flash= into display text, from
// a FIXED set of codes only (SECURITY).
func resolveIncidentFlash(r *http.Request) string {
	q := r.URL.Query()
	switch q.Get("flash") {
	case "ack":
		return "Incident acknowledged by " + auditUser(r)
	case "silenced":
		forVal := q.Get("for")
		if _, ok := incidentSilenceDurationSeconds(forVal); ok {
			return "Silenced for " + forVal
		}
	}
	return ""
}

// fleetIncidentHandler serves GET /fleet/incidents/{id}: member alerts, the full structured
// timeline.
func fleetIncidentHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		data, err := buildIncidentDetailPageData(r, d, id, incidentDetailOptions{Flash: resolveIncidentFlash(r)})
		if err != nil {
			renderIncidentNotFound(w, r, d)
			return
		}
		if err := renderIncidentDetailPage(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetIncidentAckHandler serves POST /fleet/incidents/{id}/ack (admin + CSRF,
// fleetAdminMutation). actor is the signed-in web user's own name (auditUser).
func fleetIncidentAckHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderIncidentNotFound(w, r, d)
			return
		}
		inc, err := fleet.Incident(id)
		if err != nil {
			renderIncidentNotFound(w, r, d)
			return
		}
		if inc.State == "resolved" {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{Flash: "resolved incidents cannot be acknowledged", FlashErr: true}, http.StatusBadRequest)
			return
		}
		actor := auditUser(r)
		if err := fleet.AckIncident(id, actor); err != nil {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{Flash: err.Error(), FlashErr: true}, fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.incident.ack", id, "", actor)
		// ?flash=ack is a fixed code, not the actor's name -- the GET handler
		// (resolveIncidentFlash) recomputes "by <you>" from the CURRENT session on render.
		http.Redirect(w, r, "/fleet/incidents/"+id+"?flash=ack", http.StatusSeeOther)
	}
}

// fleetIncidentSilenceHandler serves POST /fleet/incidents/{id}/silence (admin + CSRF,
// fleetAdminMutation): validates the posted "for" duration.
func fleetIncidentSilenceHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		id := r.PathValue("id")
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderIncidentNotFound(w, r, d)
			return
		}
		inc, err := fleet.Incident(id)
		if err != nil {
			renderIncidentNotFound(w, r, d)
			return
		}
		if err := r.ParseForm(); err != nil {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{Flash: "invalid form", FlashErr: true}, http.StatusBadRequest)
			return
		}
		forRaw := r.FormValue("for")
		comment := r.FormValue("comment")
		durSec, ok := incidentSilenceDurationSeconds(forRaw)
		if !ok {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{
				SilenceErr: "choose a valid duration (30m, 1h, 4h, 24h)", ForValue: forRaw, CommentValue: comment,
			}, http.StatusBadRequest)
			return
		}
		matchers := openMemberMatchers(inc)
		if len(matchers) == 0 {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{
				SilenceErr: "no open members to silence", ForValue: forRaw, CommentValue: comment,
			}, http.StatusBadRequest)
			return
		}
		actor := auditUser(r)
		now := time.Now().Unix()
		created, err := fleet.CreateSilence(core.Silence{
			Matchers: matchers,
			Start:    now,
			End:      now + durSec,
			Author:   actor,
			Comment:  comment,
		})
		if err != nil {
			renderIncidentMutationError(w, r, d, id, incidentDetailOptions{
				SilenceErr: err.Error(), ForValue: forRaw, CommentValue: comment,
			}, fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.incident.silence", id, "", fmt.Sprintf("silence=%s for=%s by=%s", created.ID, forRaw, actor))
		// ?flash=silenced&for=<forRaw> -- forRaw is already validated above against the fixed
		// duration allowlist, and resolveIncidentFlash re-validates it independently on render.
		http.Redirect(w, r, "/fleet/incidents/"+id+"?flash=silenced&for="+url.QueryEscape(forRaw), http.StatusSeeOther)
	}
}
