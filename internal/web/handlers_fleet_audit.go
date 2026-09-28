// handlers_fleet_audit.go (task C5, fleet phase 2 web UI plan C): GET
// /fleet/audit -- the fleet audit log, over core.FleetAPI.Audit(limit)
// (internal/core/fleet.go). Admin-only end to end (RoleAdmin at the route,
// routes.go; fleetGateHTML here for "master only, else 404" -- exactly like
// the rest of "Monitor"/"Admin"), unlike Managed config/Alerting/Admin,
// which stay viewer-readable: this page has no viewer-facing read at all
// (task-5-brief.md's route list is a single admin GET).
//
// Filters (?actor=, ?action=) are GET params, applied IN MEMORY over one
// Audit(limit=fleetAuditQueryLimit) read (task-5-brief.md's exact ruling),
// same convention buildFleetRows/fleetFilterMatch use for the roster
// table -- there is no server-side filtered query on core.FleetAPI.Audit
// itself, only a limit. Paginated 50/page (fleetIncidentsPageSize, this
// package's one shared page-size constant -- global-constraints.md: "Lists
// are paginated (50 per page)"). Every displayed time is the master's own
// local zone (silenceTimeText, task C4's shared helper) with its
// abbreviation shown; Detail is rendered through html/template's normal
// auto-escaping like everything else on this page -- no template.HTML
// anywhere in this file.
package web

import (
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetAuditQueryLimit is how many of the most recent audit entries this
// page reads before filtering/paginating in memory -- task-5-brief.md's
// exact value ("applied in memory over Audit(limit=5000)"). Filtering
// narrows this same window; a filter matching something older than the
// 5000th-most-recent entry simply won't show it, exactly like `fleet audit`
// CLI's own --limit.
const fleetAuditQueryLimit = 5000

// AuditRow is one row of the audit table: core.AuditEntry's fields
// formatted for display. Time is rendered through silenceTimeText (task
// C4's shared "master's own local zone, with abbreviation" helper) rather
// than a bare timestamp -- every datetime this page shows follows that same
// rule.
type AuditRow struct {
	TimeText string
	Actor    string
	Action   string
	Target   string
	Detail   string
}

func newAuditRow(e core.AuditEntry) AuditRow {
	return AuditRow{
		TimeText: silenceTimeText(e.TS),
		Actor:    e.Actor,
		Action:   e.Action,
		Target:   e.Target,
		Detail:   e.Detail,
	}
}

// auditQuery is GET /fleet/audit's parsed query: Actor/Action filters plus
// Page, mirroring incidentQuery's shape (handlers_fleet.go) but over
// core.AuditEntry's own fields.
type auditQuery struct {
	Actor  string
	Action string
	Page   int
}

func parseAuditQuery(r *http.Request) auditQuery {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	return auditQuery{
		Actor:  strings.TrimSpace(q.Get("actor")),
		Action: strings.TrimSpace(q.Get("action")),
		Page:   page,
	}
}

// encode rebuilds q's own query string (actor/action, never page -- callers
// append their own page= when building a specific page's link), for the
// filter form's action and the pagination links' shared prefix.
func (q auditQuery) encode() string {
	v := url.Values{}
	if q.Actor != "" {
		v.Set("actor", q.Actor)
	}
	if q.Action != "" {
		v.Set("action", q.Action)
	}
	return v.Encode()
}

// filterAuditEntries returns the subset of all matching q's Actor/Action
// (case-insensitive exact match on Actor -- audit actors are account names/
// "cli", not free text to substring-search; Action is matched exactly too,
// since it's a closed, machine-chosen vocabulary like "fleet.node.rename",
// never something an operator would partially remember). Both filters are
// ANDed when both are set.
func filterAuditEntries(all []core.AuditEntry, q auditQuery) []core.AuditEntry {
	if q.Actor == "" && q.Action == "" {
		return all
	}
	out := make([]core.AuditEntry, 0, len(all))
	for _, e := range all {
		if q.Actor != "" && !strings.EqualFold(e.Actor, q.Actor) {
			continue
		}
		if q.Action != "" && !strings.EqualFold(e.Action, q.Action) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// paginateAuditEntries slices all (already filtered, newest-first per
// core.FleetAPI.Audit's own contract) into page's 50-row window, clamping
// page into [1, totalPages] first -- mirrors paginateIncidents/
// paginateSilences exactly, over core.AuditEntry.
func paginateAuditEntries(all []core.AuditEntry, page int) (pageItems []core.AuditEntry, totalPages, clampedPage int) {
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

// AuditPageData is what templates/fleet_audit.html's "content" block
// renders against.
type AuditPageData struct {
	PageData

	Actor  string
	Action string

	Rows       []AuditRow
	Page       int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevHref   string
	NextHref   string
}

func buildAuditPageData(r *http.Request, d Deps) AuditPageData {
	q := parseAuditQuery(r)
	var all []core.AuditEntry
	if fleet, err := fleetAPIFor(d); err == nil {
		all, _ = fleet.Audit(fleetAuditQueryLimit)
	}
	filtered := filterAuditEntries(all, q)
	pageItems, totalPages, page := paginateAuditEntries(filtered, q.Page)
	rows := make([]AuditRow, 0, len(pageItems))
	for _, e := range pageItems {
		rows = append(rows, newAuditRow(e))
	}
	qs := q.encode()
	pageHref := func(p int) string {
		href := "/fleet/audit?page=" + strconv.Itoa(p)
		if qs != "" {
			href += "&" + qs
		}
		return href
	}
	return AuditPageData{
		PageData:   newPageData(r, d, "Audit log", "Who did what to the fleet, and when"),
		Actor:      q.Actor,
		Action:     q.Action,
		Rows:       rows,
		Page:       page,
		TotalPages: totalPages,
		HasPrev:    page > 1,
		HasNext:    page < totalPages,
		PrevHref:   pageHref(page - 1),
		NextHref:   pageHref(page + 1),
	}
}

// fleetAuditPageHandler serves GET /fleet/audit: admin-only (RoleAdmin at
// the route, routes.go), master-only (fleetGateHTML).
func fleetAuditPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		data := buildAuditPageData(r, d)
		tmpl, err := template.New("base.html").Funcs(funcMap).
			ParseFS(templatesFS, "templates/base.html", "templates/fleet_audit.html")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "base.html", data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
