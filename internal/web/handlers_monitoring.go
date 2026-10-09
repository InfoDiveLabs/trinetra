package web

import (
	"html/template"
	"log"
	"net/http"
)

// MonitoringPageData is what templates/monitoring.html renders against: the shared PageData
// (nav/topbar/CSRF) embedded, the live MonitoringView.
type MonitoringPageData struct {
	PageData
	View MonitoringView

	// ContainersUp/ContainersTotal back the "Containers up" count tile.
	ContainersUp    int
	ContainersTotal int
	// DisksWarnCrit is how many of View.Disks are at/above DiskWarnPct, for the "Filesystem
	// warning+crit" count tile.
	DisksWarnCrit int
	// StatusPick is set when the viewer can add rows to the status page.
	StatusPick *statusPick
}

// monitoringMemBarNormalMiB is the MemMiB value templates/monitoring.html's
// container/process memory meters treat as a "full" (100%) bar.
const monitoringMemBarNormalMiB = 1024.0

// memBarPct is templates/monitoring.html's helper (registered in funcMap, templates.go) for
// a container/process memory meter's bar width.
func memBarPct(memMiB float64) float64 {
	pct := memMiB / monitoringMemBarNormalMiB * 100
	if pct > 100 {
		return 100
	}
	if pct < 0 {
		return 0
	}
	return pct
}

// buildMonitoringPageData assembles MonitoringPageData from Deps: the live
// snapshot (Deps.API.Monitoring(), core.API's projection of the daemon's live
// state into a MonitoringView) plus the summary counts its top-of-page tiles
// need.
func buildMonitoringPageData(r *http.Request, d Deps) MonitoringPageData {
	var view MonitoringView
	if api := apiFor(r, d); api != nil {
		v, err := api.Monitoring()
		if err != nil {
			log.Printf("web: monitoring API.Monitoring: %v", err)
		} else {
			view = v
		}
	}

	up, total := 0, 0
	for _, c := range view.Containers {
		total++
		if c.State == "running" {
			up++
		}
	}
	warnCrit := view.DisksWarnCritCount()

	return MonitoringPageData{
		PageData:        newPageData(r, d, "Monitoring", "Containers · services · filesystems · processes"),
		View:            view,
		ContainersUp:    up,
		ContainersTotal: total,
		DisksWarnCrit:   warnCrit,
		StatusPick:      buildStatusPick(r, d),
	}
}

// renderMonitoringPage renders templates/monitoring.html through the full app-shell layout
// (base.html), the same parse/execute shape renderPageStatus.
func renderMonitoringPage(w http.ResponseWriter, data MonitoringPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/monitoring.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// monitoringHandler renders GET /monitoring: per-entity tables for containers, systemd
// units, processes, and filesystems, each row wired to the existing detail drawer.
func monitoringHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildMonitoringPageData(r, d)
		if err := renderMonitoringPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
