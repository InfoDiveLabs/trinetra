package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// publicSettingsMutation composes requireRole(RoleAdmin, ...) with requireCSRF, mirroring
// configMutation/channelsMutation: only an admin session may POST /settings/public.
func publicSettingsMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// publicPanelValue resolves ONE panel id against the live snapshot, reporting the display
// label/value/sub-text plus whether the metric is currently available.
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

// publicPanelView is one rendered tile on /public -- also the exact shape marshaled onto
// the wire by GET /public/events (sse.go's buildPublicSSEFrame).
type publicPanelView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
	Sub   string `json:"sub,omitempty"`
}

// buildPublicPanels is the ONLY place /public's handler (and its SSE counterpart,
// publicEventsHandler in sse.go) turns config + snapshot into rendered tiles.
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

// publicPanelsContain reports whether id is named in allowlist -- used for "availability",
// the one panel buildPublicPanels can't render as a flat tile (see its doc).
func publicPanelsContain(allowlist []string, id string) bool {
	return publicPanelSet(allowlist)[id]
}

// PublicPageData is what templates/public.html renders against.
type PublicPageData struct {
	BarePageData
	Panels []publicPanelView
	// ShowAvailability/Availability are populated (and the 24h strip rendered) only when
	// "availability" is in cfg.Public.Panels.
	ShowAvailability bool
	Availability     Availability
}

// renderPublicPage renders templates/public.html through the bare/centered layout
// (base_bare.html) -- like login/enroll.
func renderPublicPage(w http.ResponseWriter, data PublicPageData) error {
	tmpl, err := template.New("base_bare.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base_bare.html", "templates/public.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base_bare.html", data)
}

// publicPageHandler renders the anonymous status page -- reached only via rootHandler's
// (routes.go) branch for an anonymous request when cfg.Public.Enabled is true.
func publicPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cache-Control: no-store on every response this handler produces (enabled or not) so a
		// caching proxy/CDN in front of this daemon can never keep serving a stale rendered page.
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
			BarePageData:     newBarePageData(r, "Status"),
			Panels:           buildPublicPanels(cfg.Public.Panels, snap),
			ShowAvailability: publicPanelsContain(cfg.Public.Panels, "availability"),
			Availability:     snap.Availability,
		}
		if pub, ok := publicStatusData(d); ok {
			sdata := StatusPublicPageData{PublicPageData: data, Status: pub, OverallLabel: overallLabel[pub.Overall], Summary: statusSummary(pub.Services), Groups: groupPublicServices(pub.Services)}
			sdata.Title = pub.Title
			renderBareStatusPage(w, "status_public.html", sdata)
			return
		}
		if err := renderPublicPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// publicCatalogEntry is one row of the FIXED (non-disk) panel catalog
// /settings/public offers, in the admin picker's display order.
var publicCatalogEntry = []struct{ ID, Label, Desc string }{
	{"availability", "Availability", "24h uptime strip"},
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

// buildPublicSettingsRows lists every panel the admin picker offers: the fixed catalog
// above, plus one row per disk mount.
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

// PublicSettingsPageData is what templates/public_settings.html renders against.
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

// publicSettingsPageHandler renders GET /settings/public. requireRole(RoleAdmin, ...)
// (routes.go's wiring) has already gated this by the time it runs.
func publicSettingsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildPublicSettingsPageData(r, d)
		if err := renderPublicSettingsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// publicSettingsSaveHandler handles POST /settings/public: validates the posted enabled
// flag + checked panel ids against a clone of the current config via config.Config.Set.
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

// rootHandler serves GET /{$} (routes.go): the landing route, branching on whether the
// request carries a resolved session (userFromContext).
func rootHandler(d Deps) http.HandlerFunc {
	dashboard := dashboardHandler(d)
	public := publicPageHandler(d)
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := userFromContext(r); ok {
			dashboard(w, r)
			return
		}
		if d.Cfg().Public.Enabled {
			public(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// publicRouteRedirectHandler serves the now-canonicalized GET /public: old links/bookmarks
// into the anonymous status page still work, they just land on / (rootHandler above).
func publicRouteRedirectHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/", http.StatusMovedPermanently)
}
