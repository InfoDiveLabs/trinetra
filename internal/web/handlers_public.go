//go:build web

package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// publicRefreshSeconds is how often the anonymous /public page meta-refreshes
// itself. There is no SSE/JS polling on this page (see the design note atop
// publicPageHandler): a signed-out visitor's browser only ever gets a fresh
// render by reloading, and a plain <meta http-equiv="refresh"> is the
// simplest way to do that without adding any new anonymous-reachable data
// endpoint (an endpoint is extra attack surface this task's security
// checklist doesn't need to accept).
const publicRefreshSeconds = 30

// publicSettingsMutation composes requireRole(RoleAdmin, ...) with
// requireCSRF, mirroring configMutation/channelsMutation: only an admin
// session may POST /settings/public, and only with a valid CSRF token.
func publicSettingsMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// publicPanelValue resolves ONE panel id against the live snapshot,
// reporting the display label/value/sub-text plus whether the metric is
// currently available. This is the only function that ever turns a panel id
// into rendered text — buildPublicPanels (below) is the only caller, and it
// only ever invokes this for ids drawn from cfg.Public.Panels (the
// server-validated allowlist — see config.validatePublicPanel), never for
// every metric the snapshot happens to carry. That ordering (iterate the
// allowlist, look up each id) rather than the reverse (iterate the
// snapshot, hide what's not allowed) is what makes the allowlist an actual
// security boundary instead of a display filter: a panel id absent from
// public.panels is never even passed to this function, so there is no code
// path by which its value could reach the anonymous page.
//
// ok=false means "omit this tile" (e.g. a disk mount that no longer exists,
// or TempC==0 meaning no thermal sensor was found — DashboardView's own
// doc'd sentinel for that) — the caller must never render a zero/blank
// value in that case.
func publicPanelValue(id string, snap DashboardView) (label, value, sub string, ok bool) {
	if strings.HasPrefix(id, "disk:") {
		mount := strings.TrimPrefix(id, "disk:")
		for _, dv := range snap.Disks {
			if dv.Mount == mount {
				return "Disk " + mount, fmt.Sprintf("%.0f%%", dv.UsagePct), humanBytes(dv.FreeBytes) + " free", true
			}
		}
		return "", "", "", false
	}
	switch id {
	case "cpu":
		return "CPU", fmt.Sprintf("%.0f%%", snap.CPU), "", true
	case "mem":
		return "Memory", fmt.Sprintf("%.0f%%", snap.MemPct), "", true
	case "swap":
		return "Swap", fmt.Sprintf("%.0f%%", snap.SwapPct), "", true
	case "load":
		return "Load average", fmt.Sprintf("%.2f", snap.Load1), fmt.Sprintf("5m %.2f · 15m %.2f", snap.Load5, snap.Load15), true
	case "temp":
		if snap.TempC <= 0 {
			return "", "", "", false
		}
		return "Temperature", fmt.Sprintf("%.0f°C", snap.TempC), "", true
	case "uptime":
		status := "Operational"
		if !snap.Online {
			status = "Offline"
		}
		return "Status", status, "", true
	case "services":
		if snap.UnitsTotal <= 0 {
			return "", "", "", false
		}
		return "Services", fmt.Sprintf("%d/%d up", snap.UnitsTotal-snap.UnitsFailed, snap.UnitsTotal), "", true
	case "containers":
		if snap.ContainersTotal <= 0 {
			return "", "", "", false
		}
		return "Containers", fmt.Sprintf("%d/%d running", snap.ContainersRunning, snap.ContainersTotal), "", true
	case "net":
		if snap.NetRxBps <= 0 && snap.NetTxBps <= 0 {
			return "", "", "", false
		}
		return "Network", humanRate(snap.NetRxBps) + " ↓", humanRate(snap.NetTxBps) + " ↑", true
	}
	return "", "", "", false
}

// publicPanelView is one rendered tile on /public.
type publicPanelView struct {
	ID, Label, Value, Sub string
}

// buildPublicPanels is the ONLY place /public's handler turns config +
// snapshot into rendered tiles: it walks allowlist (cfg.Public.Panels) IN
// ORDER and, for each id, resolves it via publicPanelValue, skipping
// whatever isn't currently available. It never looks at snap directly for
// anything not named in allowlist — see publicPanelValue's doc for why that
// direction of iteration is the actual security property this task exists
// to pin.
func buildPublicPanels(allowlist []string, snap DashboardView) []publicPanelView {
	out := make([]publicPanelView, 0, len(allowlist))
	for _, id := range allowlist {
		label, value, sub, ok := publicPanelValue(id, snap)
		if !ok {
			continue
		}
		out = append(out, publicPanelView{ID: id, Label: label, Value: value, Sub: sub})
	}
	return out
}

// PublicPageData is what templates/public.html renders against. It
// deliberately does NOT embed PageData or carry a CSRF token/Role/Nav: this
// page renders for anonymous visitors (see publicPageHandler), so there is
// nothing signed-in to reflect and no mutation for a CSRF token to protect.
type PublicPageData struct {
	BarePageData
	Panels  []publicPanelView
	Refresh int
}

// renderPublicPage renders templates/public.html through the bare/centered
// layout (base_bare.html) — like login/enroll, this page has no signed-in
// session to fill an app-shell sidebar/topbar with, and unlike login/enroll
// it must never grow one even for a signed-in visitor (see the SECURITY note
// on publicPageHandler): there is no "admin view" of this route at all.
func renderPublicPage(w http.ResponseWriter, data PublicPageData) error {
	tmpl, err := template.New("base_bare.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base_bare.html", "templates/public.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base_bare.html", data)
}

// publicPageHandler renders GET /public — the ONLY unauthenticated route
// (besides /assets/*, /login, /enroll) this package serves.
//
// SECURITY (issue #67 — this route is the entire point of the task):
//   - public.enabled=false 404s rather than rendering a "disabled" page or
//     redirecting: an anonymous prober must not even learn the route exists.
//   - The rendered tiles come ONLY from buildPublicPanels(cfg.Public.Panels,
//     snapshot) — every request input (query string, headers, cookies) is
//     ignored when deciding what to show; there is no way for a caller to
//     ask for a panel outside the admin-curated list (see
//     TestPublicPageIgnoresQueryStringPanelOverride).
//   - No session is read or required, and this handler never sets a cookie —
//     an already-signed-in admin hitting /public sees exactly the same
//     anonymous page as everyone else, never their session's sidebar/topbar.
//   - renderPublicPage uses base_bare.html/PublicPageData, which carries no
//     CSRF token, role, or nav — so there is no admin/login link, form, or
//     button for the page to leak even by accident.
func publicPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cache-Control: no-store on every response this handler produces
		// (enabled or not) so a caching proxy/CDN in front of this daemon
		// can never keep serving a stale rendered page — with tiles, or the
		// mere existence of the route — after an admin disables /public or
		// narrows cfg.Public.Panels (issue #67 follow-up).
		w.Header().Set("Cache-Control", "no-store")
		cfg := d.Cfg()
		if !cfg.Public.Enabled {
			http.NotFound(w, r)
			return
		}
		var snap DashboardView
		if d.Snapshot != nil {
			snap = d.Snapshot()
		}
		data := PublicPageData{
			BarePageData: newBarePageData(r, "Status"),
			Panels:       buildPublicPanels(cfg.Public.Panels, snap),
			Refresh:      publicRefreshSeconds,
		}
		if err := renderPublicPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// publicCatalogEntry is one row of the FIXED (non-disk) panel catalog
// /settings/public offers, in the admin picker's display order.
var publicCatalogEntry = []struct{ ID, Label, Desc string }{
	{"uptime", "Status", "online/offline"},
	{"cpu", "CPU", "live %"},
	{"mem", "Memory", "live %"},
	{"swap", "Swap", "live %"},
	{"load", "Load average", "1m/5m/15m"},
	{"temp", "Temperature", "hottest sensor"},
	{"services", "Services up", "count only"},
	{"containers", "Containers", "count only"},
	{"net", "Network", "throughput"},
}

// publicSettingsRow is one checkbox tile on the /settings/public picker.
type publicSettingsRow struct {
	ID, Label, Desc string
	Checked         bool
}

// publicPanelSet builds a lookup set from a public.panels slice.
func publicPanelSet(panels []string) map[string]bool {
	m := make(map[string]bool, len(panels))
	for _, p := range panels {
		m[p] = true
	}
	return m
}

// buildPublicSettingsRows lists every panel the admin picker offers: the
// fixed catalog above, plus one row per disk mount — the UNION of mounts the
// live snapshot currently reports and mounts already named in
// cfg.Public.Panels (so a previously-curated mount that's temporarily
// missing from the snapshot doesn't just vanish from the form and get
// silently dropped on the next save), mirroring configTargetRows'
// (handlers_config.go) same union approach for the monitors table.
func buildPublicSettingsRows(panels []string, snap DashboardView) []publicSettingsRow {
	checked := publicPanelSet(panels)
	rows := make([]publicSettingsRow, 0, len(publicCatalogEntry)+len(snap.Disks))
	for _, e := range publicCatalogEntry {
		rows = append(rows, publicSettingsRow{ID: e.ID, Label: e.Label, Desc: e.Desc, Checked: checked[e.ID]})
	}

	mounts := map[string]bool{}
	for _, dv := range snap.Disks {
		mounts[dv.Mount] = true
	}
	for id := range checked {
		if strings.HasPrefix(id, "disk:") {
			mounts[strings.TrimPrefix(id, "disk:")] = true
		}
	}
	sortedMounts := make([]string, 0, len(mounts))
	for m := range mounts {
		sortedMounts = append(sortedMounts, m)
	}
	sort.Strings(sortedMounts)
	for _, m := range sortedMounts {
		id := "disk:" + m
		rows = append(rows, publicSettingsRow{ID: id, Label: "Disk " + m, Desc: "usage %", Checked: checked[id]})
	}
	return rows
}

// PublicSettingsPageData is what templates/public_settings.html renders
// against.
type PublicSettingsPageData struct {
	PageData
	Enabled bool
	Panels  []publicSettingsRow
}

func buildPublicSettingsPageData(r *http.Request, d Deps) PublicSettingsPageData {
	cfg := d.Cfg()
	var snap DashboardView
	if d.Snapshot != nil {
		snap = d.Snapshot()
	}
	return PublicSettingsPageData{
		PageData: newPageData(r, d, "Public view", "Choose what the world sees"),
		Enabled:  cfg.Public.Enabled,
		Panels:   buildPublicSettingsRows(cfg.Public.Panels, snap),
	}
}

func renderPublicSettingsPage(w http.ResponseWriter, data PublicSettingsPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/public_settings.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// publicSettingsPageHandler renders GET /settings/public.
// requireRole(RoleAdmin, ...) (routes.go's wiring) has already gated this by
// the time it runs.
func publicSettingsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildPublicSettingsPageData(r, d)
		if err := renderPublicSettingsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// publicSettingsSaveHandler handles POST /settings/public: validates the
// posted enabled flag + checked panel ids against a clone of the current
// config via config.Config.Set (reusing validatePublicPanel — the same
// "bad value -> 400, no write" contract as /config/​/channels), and only once
// both fields pass persists + in-process applies via Deps.Reload, auditing
// whatever actually changed.
//
// Unchecked checkboxes never submit (plain HTML form semantics), so an
// admin unchecking every panel posts no "panel" values at all — that must
// clear public.panels to empty (see
// TestPublicSettingsSaveUncheckingAllPanelsClearsThem), not leave the old
// list in place; joining r.Form["panel"] (empty slice when absent) straight
// into config.Set("public.panels", ...) does exactly that.
func publicSettingsSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		oldCfg := d.Cfg()
		newCfg, err := cloneConfig(oldCfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := newCfg.Set("public.enabled", boolFormValue(r, "enabled")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := newCfg.Set("public.panels", strings.Join(r.Form["panel"], ",")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := d.Reload(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		oldEnabled, newEnabled := strconv.FormatBool(oldCfg.Public.Enabled), strconv.FormatBool(newCfg.Public.Enabled)
		if oldEnabled != newEnabled {
			logAudit(d, r, "public.set", "public.enabled", oldEnabled, newEnabled)
		}
		oldPanels, newPanels := strings.Join(oldCfg.Public.Panels, ","), strings.Join(newCfg.Public.Panels, ",")
		if oldPanels != newPanels {
			logAudit(d, r, "public.set", "public.panels", oldPanels, newPanels)
		}

		data := buildPublicSettingsPageData(r, d)
		if err := renderPublicSettingsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
