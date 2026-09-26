package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetSkewWarnSec mirrors the CLI's clock-skew warning cutoff
// (internal/trinetra/fleet_cmd.go's skewWarnCLI = 30): internal/web can't
// import internal/trinetra (one-way module graph, see server.go's Deps
// doc), so this is an independent literal with the same value, exactly like
// nodeDurText/nodeAgoText already duplicate that package's own `ago` helper
// for the same layering reason.
const fleetSkewWarnSec = 30

// fleetQuery is /fleet's (and /fleet/table's and /api/fleet/nodes')
// resolved query string: the three core.NodeFilter criteria plus the
// table's sort column/direction. Every field is the empty string/"asc" by
// default (no filter, default sort).
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

// encode renders fq back to a URL query string (tag/state/q included only
// when set; sort/dir included only when a sort column is chosen), used both
// for the htmx poll fragment's "keep the current query string" requirement
// (task-5-brief.md) and for building each sortable column header's link.
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

// fleetStateMatches reports whether a node's State satisfies fq's state
// filter, with one deliberate, documented deviation from plain
// core.NodeFilter.Match semantics: the health strip's "Behind" count links
// to "?state=lagging" (task-5-brief.md's ruling: "For 'behind', use
// ?state=lagging and include stale too; document that" -- NodeSummary.State
// has no combined "behind" value of its own), so a want of exactly
// "lagging" also matches a node whose State is "stale". Every other want
// value (including "" -- no filter) is an ordinary exact match, identical
// to core.NodeFilter.Match's own State comparison. This is intentionally
// NOT applied to /api/fleet/nodes (fleetNodesAPIHandler uses plain
// core.NodeFilter.Match there instead): the JSON API's contract is "matches
// Nodes(filter)" verbatim, so it stays a faithful passthrough of
// core.NodeFilter's real semantics; only the HTML page/table's "Behind"
// convenience link gets the OR.
func fleetStateMatches(want, got string) bool {
	if want == "" {
		return true
	}
	if want == "lagging" {
		return got == "lagging" || got == "stale"
	}
	return want == got
}

// fleetHealth is the /fleet health strip's four counts, over the FULL
// (unfiltered) roster -- a fleet-wide summary that stays stable regardless
// of whatever tag/state/q filter the table below it is currently showing,
// the same convention the rest of the app uses for summary tiles versus
// filtered tables (e.g. monitoring.html's count tiles versus its
// data-filter search).
type fleetHealth struct {
	Online, Behind, Down, Revoked int
}

// computeFleetHealth buckets nodes by NodeSummary.State per the ruling:
// online, lagging+stale combined as "Behind", down, revoked. A state
// outside this set (shouldn't happen; defensive) counts toward none of the
// four tiles rather than panicking or guessing.
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

// FleetRow is one row of the /fleet table: NodeSummary plus the
// display-only fields the template needs and can't compute itself
// (html/template has no arithmetic/formatting beyond funcMap helpers).
type FleetRow struct {
	core.NodeSummary
	// Href is this row's link target: "/" for self, "/n/<id>/" for a
	// remote node (task-5-brief.md: "Rows link to /n/<id>/ (self links to
	// /)"), built via nodeHref exactly like every other node-aware link in
	// this package.
	Href string
	// SkewText is fmtSkew's exact rendering (internal/trinetra/fleet_cmd.go,
	// mirrored here since internal/web can't import that package): "-" for
	// self, "0s" for zero skew, else a signed "+Ns"/"-Ns".
	SkewText string
	// SkewWarn is the CLI's clock-skew warning sentence verbatim
	// ("clock differs from this master's by <±Ns> (fix NTP on that host)",
	// internal/trinetra/fleet_cmd.go's printNodeWarnings), non-empty only
	// when |SkewSec| > fleetSkewWarnSec and the node isn't self.
	SkewWarn string
	// DropsWarn is the CLI's replica-drops warning sentence ("replica
	// drops: N out of order, N over the series limit, N duplicates
	// (harmless re-sends)", task-5-brief.md's exact wording), non-empty
	// only when any of the three drop counters is nonzero and the node
	// isn't self.
	DropsWarn string
	// OutboxText is humanBytes(OutboxBytes), empty when nothing is queued
	// (OutboxBytes<=0) so the Link column doesn't render a bare "0 B" for
	// every healthy node.
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

// fleetSkewWarnText renders the CLI's clock-skew warning sentence (see
// FleetRow.SkewWarn's doc) for n, or "" when it doesn't apply. Verbatim
// against internal/trinetra/fleet_cmd.go's printNodeWarnings (round-1
// review of this task: the "(fix NTP on that host)" suffix was originally
// dropped, making the wording not actually verbatim -- restored here).
func fleetSkewWarnText(n core.NodeSummary) string {
	if n.Self || (n.SkewSec <= fleetSkewWarnSec && n.SkewSec >= -fleetSkewWarnSec) {
		return ""
	}
	return fmt.Sprintf("clock differs from this master's by %s (fix NTP on that host)", fleetSkewText(n))
}

// fleetDropsWarnText renders the CLI's replica-drops warning sentence (see
// FleetRow.DropsWarn's doc) for n, or "" when it doesn't apply.
func fleetDropsWarnText(n core.NodeSummary) string {
	if n.Self || (n.DroppedOutOfOrder == 0 && n.DroppedCardinality == 0 && n.DroppedDuplicate == 0) {
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

// fleetSortKeys are the table's sortable columns, in header order --
// fleetSortLinks builds one link per key (a th's href), sortFleetNodes
// switches on the same set.
var fleetSortKeys = []string{"name", "state", "cpu", "mem", "disk", "load", "version", "lastseen"}

// fleetSortLinks builds, for every sortable column, the href that column's
// header link should carry: the current tag/state/q filter preserved,
// sort=<key>, and dir toggled to "desc" only when that column is already
// the active ascending sort (clicking an inactive column always starts
// ascending, matching th's mockup-era convention elsewhere of "click again
// to reverse").
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

// sortFleetNodes sorts nodes in place by sortKey/dir (task-5-brief.md:
// "?sort=cpu&dir=desc sorts"). An unrecognized/empty sortKey falls back to
// the brief's stated default: down nodes first, then name ascending -- the
// same "surface trouble first" convention the rest of the dashboard uses
// (e.g. the monitoring page's bad/warnc count tiles).
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

// fetchFleetNodes returns the full, unfiltered node roster via r's
// request-scoped fleetMemo (fleet_memo.go) -- the SAME cached
// Fleet().Nodes(core.NodeFilter{}) result resolveMasterAndNodes/fleetRole/
// navCountsFor may already have fetched earlier in this request, so this
// costs a real round trip only when nothing else in the request already
// paid for one. Degrades a nil Deps.Fleet, a nil FleetAPI, or a read error
// to an empty roster rather than failing the page -- this daemon has
// already been proven a master by the caller (fleetOverviewHandler et al.)
// before this runs, but a transient Nodes() error still shouldn't 500 an
// otherwise-working page.
func fetchFleetNodes(r *http.Request, d Deps) []core.NodeSummary {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return nil
	}
	return nodes
}

// fleetMetric is the heatmap's metric selector (?metric=cpu|mem|disk|load,
// task-1a-brief.md), defaulting to cpu.
type fleetMetric string

const (
	fleetMetricCPU  fleetMetric = "cpu"
	fleetMetricMem  fleetMetric = "mem"
	fleetMetricDisk fleetMetric = "disk"
	fleetMetricLoad fleetMetric = "load"
)

// fleetMetricLabels is the metric selector's link order/labels, top to
// bottom -- also fleetMetricOptions' iteration order, so the chips render
// CPU, Memory, Disk, Load regardless of Go map ordering.
var fleetMetricLabels = []struct {
	key   fleetMetric
	label string
}{
	{fleetMetricCPU, "CPU"},
	{fleetMetricMem, "Memory"},
	{fleetMetricDisk, "Disk"},
	{fleetMetricLoad, "Load"},
}

// parseFleetMetric reads ?metric= off r's query string, defaulting to cpu
// for anything absent/unrecognized -- the same "unknown value degrades to
// the default" convention parseFleetQuery's dir field already uses.
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

// FleetMetricOption is one entry in the heatmap's metric selector -- a
// plain link (task-1a-brief.md: "The metric selector is links, not a
// form"), not a <select>/radio group.
type FleetMetricOption struct {
	Label  string
	Href   string
	Active bool
}

// fleetMetricOptions builds the metric selector's four options in
// fleetMetricLabels' order, each link carrying fq's other query params
// (tag/state/q/sort/dir) via fleetPageQueryString.
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

// fleetHeatBand is the heatmap tile's discrete colour band. The controller
// ruling: below 70% is neutral (graphite/slate, .heat-neutral), 70-85% is
// amber at low intensity (.heat-amber-low), 85%+ is amber at FULL intensity
// with bold text (.heat-amber-full) -- ember is NEVER used for a metric
// value, no matter how hot. fleetHeatDown/fleetHeatRevoked override the
// metric ramp entirely for a node that isn't reporting a normal value: a
// down tile gets an ember OUTLINE (the one place this ramp touches ember,
// matching the rest of the app's "ember is down, nothing else" rule) plus
// the literal text "down"; a revoked tile stays neutral with the text
// "revoked".
type fleetHeatBand string

const (
	fleetHeatNeutral   fleetHeatBand = "neutral"
	fleetHeatAmberLow  fleetHeatBand = "amber-low"
	fleetHeatAmberFull fleetHeatBand = "amber-full"
	fleetHeatDown      fleetHeatBand = "down"
	fleetHeatRevoked   fleetHeatBand = "revoked"
)

// pctHeatBand buckets a 0-100 percentage metric (CPU/mem/disk) into the
// controller ruling's three numeric bands.
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

// Load1's absolute heatmap thresholds (task-1a-brief.md: "Load uses the
// thresholds 1, 2 and 4 per core only if core count is available in
// NodeSummary; otherwise use absolute load thresholds 1, 4 and 8, and
// document the choice"). core.NodeSummary carries no core-count field at
// all (see its doc in internal/core/fleet.go), so the per-core ramp is never
// reachable here and this package always uses the absolute numbers.
//
// fleetLoadAmberLowAt (1) is "one core's worth of runnable work" -- below it
// every tile stays neutral regardless of the box's real size.
// fleetLoadAmberFullAt (4) is where a small (roughly 4-core) box is already
// saturated -- at or above it the tile goes to the ramp's top band.
//
// The brief's third number, 8 ("thresholds 1, 4 and 8"), is kept here as a
// named, documented "severely overloaded" reference point, but it does NOT
// get a fourth visual tier of its own: the controller ruling caps the
// metric ramp at three bands (neutral/amber-low/amber-full) and reserves
// ember for "down" alone, so both the 4-8 and the 8+ range render
// identically (amber-full, bold) -- the same way the per-core ramp's own
// >4-per-core region would have had nowhere further to go past its own top
// band either.
const (
	fleetLoadAmberLowAt  = 1.0
	fleetLoadAmberFullAt = 4.0
	// fleetLoadSevereAt is intentionally unused by loadHeatBand -- see the
	// doc above.
	fleetLoadSevereAt = 8.0
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

// fleetHeatValue resolves metric's band and display text for n. A down or
// revoked node short-circuits to its own band/text regardless of metric
// (task-1a-brief.md: "Down and revoked nodes show their state instead of
// the value ... Each tile ALWAYS shows the node name and the numeric value
// as text, so colour is never the only signal" -- for these two states, the
// state word itself IS that text).
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

// buildFleetHeatTiles builds one FleetHeatTile per node in nodes (already
// filtered by the page's current tag/state/q, see fleetFilterNodes), sorted
// by name for a stable order that doesn't depend on the table's own current
// sort column.
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

// fleetTopN is the top-N panels' row count (task-1a-brief.md: "top 5 by
// CPU, top 5 by memory, top 5 by worst disk").
const fleetTopN = 5

// FleetTopRow is one row in a top-N ranking panel: reuses
// templates/dashboard.html's containerBar shape (Name/ValueText/WidthPct)
// so the ".hbars"/".hbar" CSS already styling that panel applies unchanged
// here, plus the row's node link.
type FleetTopRow struct {
	Name      string
	Href      string
	ValueText string
	WidthPct  float64
}

// topNByMetric ranks nodes (excluding down/revoked -- task-1a-brief.md:
// "Down and revoked nodes are excluded from the metric rankings") by value,
// descending, ties broken by name for a deterministic presentation order,
// and returns at most fleetTopN rows. value is assumed to be a 0-100
// percentage (CPU/mem/worst-disk are the only metrics this backs); WidthPct
// is the same value clamped to [0,100] for the bar's width (a multi-core
// box's aggregate CPU can exceed 100%, in which case the bar still shows
// full while ValueText keeps the real number).
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
// heatmap/top-N panels (task-1a-brief.md: "the heatmap and the panels
// respect the page's existing filters (tag/state/q, using the same
// non-admin q rule as the table)").
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
// Round-1 review of Task 5 found that /fleet?q= and /api/fleet/nodes?q=
// both filtered through core.NodeFilter.Match verbatim, whose Query
// haystack is "name + id + RemoteAddr" -- letting a viewer probe a node's
// network address by searching for address fragments, even though
// RemoteAddr is otherwise redacted from a viewer's view entirely
// (fleetNodesAPIHandler's own RemoteAddr-blanking, and the HTML table never
// rendering it at all). admin==true keeps core.NodeFilter.Match's exact
// semantics (name+id+RemoteAddr) unchanged -- an admin session already sees
// the real RemoteAddr elsewhere, so matching against it here leaks nothing
// new. admin==false instead matches Query against name+id+tags, never
// RemoteAddr; State and Tag are identical either way (neither ever touched
// RemoteAddr to begin with).
//
// This is a web-layer helper, not a change to core.NodeFilter.Match itself
// (the ruling: "don't change internal/core") -- so the non-admin path
// duplicates Match's State/Tag checks verbatim rather than trying to call
// into it.
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
	filtered := fleetFilterNodes(nodes, fq, admin)
	sortFleetNodes(filtered, fq.Sort, fq.Dir)

	rows := make([]FleetRow, 0, len(filtered))
	for _, n := range filtered {
		rows = append(rows, newFleetRow(n))
	}
	return rows
}

// FleetTableData is what templates/fleet.html's "fleet_rows" block (the
// bare <tbody> fragment /fleet/table returns) renders against -- just the
// rows plus the query string that block's self-polling tbody re-sends on
// every subsequent htmx trigger (see that block's own doc), no shell/nav/
// topbar.
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
	// Metric/MetricOptions/RefreshHref/HeatTiles/TopCPU/TopMem/TopDisk/
	// DownNow back the heatmap and top-N panels (task 1a) that sit above the
	// table -- see buildFleetPageData.
	Metric        fleetMetric
	MetricOptions []FleetMetricOption
	RefreshHref   string
	HeatTiles     []FleetHeatTile
	TopCPU        []FleetTopRow
	TopMem        []FleetTopRow
	TopDisk       []FleetTopRow
	DownNow       []FleetDownRow
}

// buildFleetPageData assembles FleetPageData for GET /fleet: the full
// roster (for the health strip), the current query's filtered/sorted rows,
// the query-string/sort-link plumbing the template needs, and (task 1a) the
// heatmap tiles and top-N panels -- built from the SAME filtered node slice
// the table itself filters from (fetchFleetNodes' single memoized Nodes()
// call, task-1a-brief.md: "Both render from the request-scoped memo's
// single Nodes() call. No extra Fleet() round trips").
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
	return FleetPageData{
		PageData:      newPageData(r, d, "Fleet", sub),
		Rows:          buildFleetRows(nodes, fq, admin),
		Health:        computeFleetHealth(nodes),
		Query:         fq,
		QueryString:   fq.encode(),
		SortLinks:     fleetSortLinks(fq),
		TotalNodes:    len(nodes),
		Metric:        metric,
		MetricOptions: fleetMetricOptions(fq, metric),
		RefreshHref:   "/fleet?" + fleetPageQueryString(fq, metric),
		HeatTiles:     buildFleetHeatTiles(filtered, metric),
		TopCPU:        topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.CPU }),
		TopMem:        topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.MemPct }),
		TopDisk:       topNByMetric(filtered, func(n core.NodeSummary) float64 { return n.WorstDiskPct }),
		DownNow:       fleetDownRows(filtered),
	}
}

// renderFleetPage renders templates/fleet.html's "content" block through
// the full app-shell layout (base.html), the same parse/execute shape
// renderMonitoringPage/renderUsersPage use for a page needing extra fields
// beyond plain PageData.
func renderFleetPage(w http.ResponseWriter, data FleetPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderFleetTableFragment renders templates/fleet.html's "fleet_rows"
// block alone -- no base.html, no "content" wrapper -- so GET /fleet/table
// returns exactly the <tbody id="fleet-tbody">...</tbody> fragment the
// polling table swaps in (task-5-brief.md: "/fleet/table returns only the
// <tbody> fragment"). The fragment's own root element carries the same id
// the page's table targets (hx-target="#fleet-tbody" hx-swap="outerHTML"
// in fleet.html), so each successive poll's replacement stays targetable by
// the next one.
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
// handler applies first (task-5-brief.md: "All 404 unless
// fleetRole(d)=='master'"), reported via renderNotFound (styled page, for
// the HTML page) or a bare http.NotFound (for the fragment/JSON endpoints,
// which have no shell to render). It returns whether the caller should stop
// (true == already handled, a 404 was written). fleetRole(r, d) reads r's
// request-scoped fleetMemo (fleet_memo.go), so this gate's own Status()
// check costs a real round trip only once per request even though both
// GET /fleet's page render AND (for the HTML path) newPageData's own
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

// fleetOverviewHandler serves GET /fleet: the health strip, filter form,
// and sortable node table (task-5-brief.md). Viewer-gated by routes.go
// (requireRole(RoleViewer, ...)) exactly like the rest of "Monitor";
// master-only-ness is enforced here, not by RBAC (a solo/child daemon's
// viewer can sign in fine, there's just no fleet to show them).
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

// fleetTableHandler serves GET /fleet/table: the htmx poll target
// (hx-trigger="every 5s" in fleet.html) that keeps the table's rows fresh
// without reloading the page shell/health strip. Same filter/sort query
// params as GET /fleet, same 404 gate.
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

// fleetNodesAPIHandler serves GET /api/fleet/nodes: the JSON roster,
// filtered by tag/state/q exactly as core.NodeFilter.Match defines it for an
// ADMIN caller (NOT fleetStateMatches' health-strip "lagging also matches
// stale" convenience -- see fleetStateMatches' doc for why the JSON API
// stays a faithful passthrough of Nodes(filter) instead). Response shape is
// []core.NodeSummary, the same type Fleet().Nodes itself returns.
//
// RemoteAddr (round-1 review): a viewer's response has every node's
// RemoteAddr blanked before encoding -- a node's network address is
// operationally sensitive (infrastructure topology), and this endpoint's
// only RBAC floor is RoleViewer (routes.go), the same floor the read-only
// HTML page/table share. Only an admin session (currentRole(r)=="admin")
// sees the real value, matching how e.g. container logs (admin-only
// entirely) treat operationally sensitive data more strictly than plain
// monitoring data.
//
// Query matching (Task 6 review carry-over): filtering itself goes through
// fleetFilterMatch, not a bare filter.Match(n) -- a non-admin caller's q=
// must never be able to confirm/deny a RemoteAddr value even indirectly
// (whether a node is present in the filtered result), matching the
// RemoteAddr redaction above. See fleetFilterMatch's doc.
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
