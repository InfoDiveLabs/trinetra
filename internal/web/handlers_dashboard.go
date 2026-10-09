package web

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
)

// activeAlertView is one row of the dashboard's "Active alerts" panel,
// projected from core.AlertRecord (the daemon's ActiveAlerts, read over the
// control socket -- see activeAlertsViaAPI). It carries only what the panel
// and the topbar status pill render, so those pages never read the daemon's
// alerts.json off disk.
type activeAlertView struct {
	Key    string
	Reason string
	Since  int64
	Acked  bool
	// Critical mirrors trinetra.ActiveAlert.Critical -- the severity of
	// whatever condition raised this alert, recorded at fire time -- so
	// callers (topbarStatus) can tell a critical alert from a mere warning
	// among currently active alerts without re-deriving it from Reason
	// text.
	Critical bool
}

// activeAlertsViaAPI returns this request's active alerts, memoized via the
// request-scoped fleetMemo (fleet_memo.go) so every call site in the same
// request -- the topbar status pill, the sidebar alert badge, the dashboard
// panel, and the alerts page -- shares one control-socket round trip rather
// than each making its own. The actual read/projection is
// fetchActiveAlertsViaAPI, below; this is just the memoized front door every
// caller already used before the memo existed, unchanged.
func activeAlertsViaAPI(r *http.Request, d Deps) []activeAlertView {
	return fleetMemoFrom(r).activeAlerts(r, d)
}

// snapshotViaAPI returns this request's apiFor(r,d).Snapshot(), memoized the
// same way as activeAlertsViaAPI above -- the dashboard page's own live view
// and the sidebar's Monitoring badge (nav_counts.go's node-scope branch) used
// to each poll a remote node's Snapshot() independently. NOT used by
// remoteNodeSnapshot (sse.go)'s polling loop, which must keep reading live for
// the SSE connection's whole lifetime rather than caching one snapshot forever.
func snapshotViaAPI(r *http.Request, d Deps) (DashboardView, error) {
	return fleetMemoFrom(r).snapshot(r, d)
}

// fetchActiveAlertsViaAPI reads the daemon's current active alerts over the
// control socket (Deps.API.ActiveAlerts) and projects each core.AlertRecord
// into an activeAlertView for the dashboard panel, the sidebar badge, the
// topbar status pill, and the alerts page. It is the channel-only path (a
// plugin must not read daemon-owned state from disk), shared by all four
// callers. Called at most once per request
// -- see activeAlertsViaAPI's own doc, above, which every caller uses
// instead of this directly.
//
// The AlertRecord shape comes from trinetra's activeAlertRecords mapping:
// the human reason text is carried in Source, and Critical is encoded as
// Severity == "critical" (Severity.String is "info"/"warning"/"critical").
// Results are ordered most-recent-first (Since desc, then Key asc), the same
// order loadActiveAlerts produced, so the rendered panels are unchanged.
//
// A nil API or a read error (a transient socket failure) degrades to nil --
// "no active alerts" -- rather than failing the whole page: these panels are
// display-only, never the source of truth for alert state (that stays the
// daemon's AlertState and the `trinetra alerts` CLI).
//
// Reads through apiFor(r, d) (node_scope.go), so a request scoped to a fleet
// node (/n/{node}/...) sees that node's own active alerts rather than the
// master's -- the topbar status pill, the sidebar alert badge, and every
// page's alert panel all follow the request's node scope through this one
// call site.
func fetchActiveAlertsViaAPI(r *http.Request, d Deps) []activeAlertView {
	api := apiFor(r, d)
	if api == nil {
		return nil
	}
	recs, err := api.ActiveAlerts()
	if err != nil {
		return nil
	}
	out := make([]activeAlertView, 0, len(recs))
	for _, rec := range recs {
		out = append(out, activeAlertView{
			Key:      rec.Key,
			Reason:   rec.Source,
			Since:    rec.Time,
			Acked:    rec.Acked,
			Critical: rec.Severity == "critical",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since != out[j].Since {
			return out[i].Since > out[j].Since
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// containerBar is one row of a "Top containers" hbar panel: a container's
// value (CPU% or MemMiB) rendered as a track width RELATIVE to the largest
// value in that same top-N list -- unlike the CPU/mem/swap resource tiles
// (which are already 0-100 percentages, usable as a width directly),
// MemMiB is an absolute megabyte figure, so it needs normalizing against
// its list's own max before it means anything as a CSS bar width.
type containerBar struct {
	Name      string
	State     string
	ValueText string
	WidthPct  float64
}

// containerBars builds a []containerBar from list, using value to extract
// the metric each row's width is normalized against (the list's own max ->
// 100%) and format to render that metric's display text (e.g. "27%" or
// "555M").
func containerBars(list []ContainerView, value func(ContainerView) float64, format func(float64) string) []containerBar {
	if len(list) == 0 {
		return nil
	}
	max := 0.0
	for _, c := range list {
		if v := value(c); v > max {
			max = v
		}
	}
	out := make([]containerBar, 0, len(list))
	for _, c := range list {
		v := value(c)
		width := 0.0
		if max > 0 {
			width = v / max * 100
		}
		out = append(out, containerBar{Name: c.Name, State: c.State, ValueText: format(v), WidthPct: width})
	}
	return out
}

// DashboardPageData is what templates/dashboard.html renders against: the
// shared PageData (nav/topbar/CSRF) embedded, the live DashboardView, the
// current active-alerts list, and the two "Top containers" panels
// pre-normalized into display-ready bars (containerBars).
type DashboardPageData struct {
	PageData
	View       DashboardView
	Host       HostSummary
	Alerts     []activeAlertView
	TopCPUBars []containerBar
	TopMemBars []containerBar
}

// buildDashboardPageData assembles DashboardPageData from Deps: the live
// snapshot (Deps.API.Snapshot(), core.API's projection of the daemon's live
// state -- see core.DashboardView's doc) plus the current active-alerts list
// (activeAlertsViaAPI, over the control socket). The topbar's status pill
// (PageData.Status/StatusText) is computed by newPageData itself from that
// same active-alert set (topbarStatus, templates.go) -- see PageData's doc
// for why every page
// shares one computation rather than this page deriving its own from
// disk/unit state.
func buildDashboardPageData(r *http.Request, d Deps) DashboardPageData {
	var view DashboardView
	// snapshotViaAPI is memoized per request, so this read is shared with
	// navCountsFor's node-scope branch (nav_counts.go) instead of each making
	// its own apiFor(r,d).Snapshot() round trip -- errNoAPI (fleet_memo.go) is
	// "no core.API to poll at all", the same case the old `if api := apiFor(r,
	// d); api != nil` guard silently skipped without logging; any other error
	// still logs exactly as before.
	v, err := snapshotViaAPI(r, d)
	if err != nil {
		if !errors.Is(err, errNoAPI) {
			log.Printf("web: dashboard API.Snapshot: %v", err)
		}
	} else {
		view = v
	}
	alerts := activeAlertsViaAPI(r, d)
	return DashboardPageData{
		PageData: newPageData(r, d, "Dashboard", "Overview · live"),
		View:     view,
		Host:     buildHostSummary(r, d),
		Alerts:   alerts,
		TopCPUBars: containerBars(view.TopCPUContainers,
			func(c ContainerView) float64 { return c.CPUPct },
			func(v float64) string { return fmt.Sprintf("%.0f%%", v) }),
		TopMemBars: containerBars(view.TopMemContainers,
			func(c ContainerView) float64 { return c.MemMiB },
			func(v float64) string { return fmt.Sprintf("%.0fM", v) }),
	}
}

// renderDashboardPage renders templates/dashboard.html through the full
// app-shell layout (base.html) against DashboardPageData -- the same
// parse/execute shape renderPageStatus (templates.go) uses for plain PageData
// pages, mirrored here (like renderUsersPage/handlers_users.go) because this
// page needs the extra View/Alerts fields alongside the shared ones.
func renderDashboardPage(w http.ResponseWriter, data DashboardPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/dashboard.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// dashboardHandler renders GET /: the live summary counts, resource tiles,
// active alerts, top-containers hbars, and filesystems table, all bound to
// the current Deps.Snapshot() -- see DashboardView's doc for the exact
// projection. requireRole(RoleViewer, ...) (routes.go's wiring) has already
// gated this by the time it runs.
func dashboardHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildDashboardPageData(r, d)
		if err := renderDashboardPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// ledClass buckets a metric value into the led/meter/badge color classes
// ("ok"/"warn"/"crit") against the given warn/crit cutoffs. Used by
// templates/dashboard.html for every tile's <span class="led ...">.
func ledClass(value, warn, crit float64) string {
	switch {
	case value >= crit:
		return "crit"
	case value >= warn:
		return "warn"
	default:
		return "ok"
	}
}

// byteUnits are the humanBytes step points, largest first (one significant
// decimal for GB and above, whole numbers below).
var byteUnits = []struct {
	size float64
	unit string
}{
	{1 << 40, "TB"},
	{1 << 30, "GB"},
	{1 << 20, "MB"},
	{1 << 10, "KB"},
}

// humanBytes formats b as e.g. "4.1 GB", "310 MB": one decimal at GB/TB
// scale, whole numbers at MB/KB scale, plain "B" below 1 KB.
func humanBytes(b uint64) string {
	f := float64(b)
	for _, u := range byteUnits {
		if f >= u.size {
			v := f / u.size
			if u.unit == "TB" || u.unit == "GB" {
				return fmt.Sprintf("%.1f %s", v, u.unit)
			}
			return fmt.Sprintf("%.0f %s", v, u.unit)
		}
	}
	return fmt.Sprintf("%d B", b)
}

// humanRate formats a bytes/sec throughput, e.g. "1.8 MB/s", "240 KB/s".
func humanRate(bps float64) string {
	switch {
	case bps >= 1<<20:
		return fmt.Sprintf("%.1f MB/s", bps/(1<<20))
	case bps >= 1<<10:
		return fmt.Sprintf("%.0f KB/s", bps/(1<<10))
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// diskFullSoonDays is the DaysToFull cutoff at/under which the filesystems
// table's trend column renders as "filling" (red, .trend.up) rather than
// "stable" (green, .trend.dn) -- a display-only cutoff, same caveat as
// DiskCriticalPct/DiskWarnPct in dashboard_view.go.
const diskFullSoonDays = 14

// diskTrendText renders a DiskView's fill-rate projection as "▲ full in Nd" /
// "▼ stable" trend column text.
func diskTrendText(days float64, known bool) string {
	if !known {
		return "▼ stable"
	}
	if days <= diskFullSoonDays {
		return fmt.Sprintf("▲ full in %.0fd", days)
	}
	return fmt.Sprintf("▼ %.0fd to full", days)
}

// diskTrendClass is diskTrendText's companion CSS class ("up"/"dn").
func diskTrendClass(days float64, known bool) string {
	if known && days <= diskFullSoonDays {
		return "up"
	}
	return "dn"
}

// loadLedClass buckets the load-1m tile's led class relative to core count:
// a load average is only "high" relative to how many cores can service it,
// unlike a flat percentage -- warn at >=70% of cores busy, crit at >=100%.
func loadLedClass(load1 float64, cores int) string {
	if cores <= 0 {
		cores = 1
	}
	c := float64(cores)
	return ledClass(load1, 0.7*c, c)
}

// subInt is templates/dashboard.html's integer subtraction helper (e.g. the
// summary count tile's "N down" = total-running); html/template has no
// built-in arithmetic.
func subInt(a, b int) int { return a - b }
