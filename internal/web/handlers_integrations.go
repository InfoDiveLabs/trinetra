package web

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func integrationsMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

func pluginManager(d Deps) plugin.Manager {
	if d.Plugins == nil {
		return nil
	}
	return d.Plugins()
}

type integrationRow struct {
	plugin.Status
	TrustLabel  string
	TrustClass  string
	StateLabel  string
	HealthLabel string
	HealthClass string
}

func newIntegrationRow(st plugin.Status) integrationRow {
	row := integrationRow{Status: st, StateLabel: string(st.State)}
	switch st.Trust {
	case plugin.TrustUnverified:
		row.TrustLabel, row.TrustClass = "Unverified", "warn"
	case plugin.TrustDev:
		row.TrustLabel, row.TrustClass = "Dev", "warn"
	default:
		row.TrustLabel, row.TrustClass = "Verified", "ok"
	}
	switch {
	case st.State != plugin.StateEnabled:
		row.HealthLabel = "not running"
	case !st.Running:
		row.HealthLabel, row.HealthClass = "stopped", "crit"
	case !st.Health.Connected:
		row.HealthLabel, row.HealthClass = "starting", "warn"
	case st.Health.Status.State == "error":
		row.HealthLabel, row.HealthClass = "error", "crit"
	case st.Health.Status.State == "degraded":
		row.HealthLabel, row.HealthClass = "degraded", "warn"
	default:
		row.HealthLabel, row.HealthClass = "healthy", "ok"
	}
	return row
}

type IntegrationsPageData struct {
	PageData
	Unavailable   bool
	Installed     []integrationRow
	Available     []plugin.CatalogEntry
	CatalogStale  bool
	CatalogNote   string
	NotConfigured bool
	Flash         string
	FlashErr      bool
}

func resolveIntegrationsFlash(r *http.Request) (string, bool) {
	switch r.URL.Query().Get("flash") {
	case "refreshed":
		return "Catalog refreshed.", false
	case "refresh-error":
		return "The catalog could not be refreshed.", true
	case "install-error":
		return "The plugin could not be installed. See the daemon log for details.", true
	case "install-bad-ref":
		return "Enter a catalog id (optionally id@version) or an https:// URL.", true
	case "install-unverified":
		return "Unverified plugins are disabled on this host (plugins.allow_unverified).", true
	case "updated":
		return "Update check finished.", false
	case "update-error":
		return "The update failed. See the daemon log for details.", true
	}
	return "", false
}

func buildIntegrationsPageData(r *http.Request, d Deps) IntegrationsPageData {
	data := IntegrationsPageData{PageData: newPageData(r, d, "Integrations", "Plugins that extend Trinetra")}
	data.Flash, data.FlashErr = resolveIntegrationsFlash(r)
	m := pluginManager(d)
	if m == nil {
		data.Unavailable = true
		return data
	}
	list, err := m.List()
	if err != nil {
		log.Printf("web: plugin list: %v", err)
	}
	installed := map[string]bool{}
	for _, st := range list {
		installed[st.ID] = true
		data.Installed = append(data.Installed, newIntegrationRow(st))
	}
	cat, err := m.Catalog(false)
	switch {
	case errors.Is(err, plugin.ErrNoRegistryKeys):
		data.NotConfigured = true
	case err != nil:
		log.Printf("web: plugin catalog: %v", err)
		data.CatalogNote = "The catalog could not be loaded."
	default:
		data.CatalogStale = cat.Stale
		for _, e := range cat.Entries {
			if !installed[e.ID] {
				data.Available = append(data.Available, e)
			}
		}
	}
	return data
}

func renderIntegrationsPageStatus(w http.ResponseWriter, page string, data any, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

func integrationsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := renderIntegrationsPageStatus(w, "integrations.html", buildIntegrationsPageData(r, d), http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func redirectIntegrations(w http.ResponseWriter, r *http.Request, target, flash string) {
	if flash != "" {
		target += "?" + url.Values{"flash": {flash}}.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// validInstallRef accepts a catalog id (id or id@version) or an https URL; local paths are CLI only.
func validInstallRef(ref string) bool {
	if strings.HasPrefix(ref, "https://") {
		u, err := url.Parse(ref)
		return err == nil && u.Host != "" && !strings.ContainsAny(ref, " \t\r\n")
	}
	id, ver, hasVer := strings.Cut(ref, "@")
	if !plugin.IDPattern.MatchString(id) {
		return false
	}
	return !hasVer || (ver != "" && !strings.ContainsAny(ver, "/\\ ") && !strings.Contains(ver, ".."))
}

func integrationsInstallHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := pluginManager(d)
		if m == nil {
			redirectIntegrations(w, r, "/integrations", "install-error")
			return
		}
		ref := strings.TrimSpace(r.FormValue("ref"))
		if !validInstallRef(ref) {
			redirectIntegrations(w, r, "/integrations", "install-bad-ref")
			return
		}
		res, err := m.Install(plugin.InstallRequest{Ref: ref, Actor: auditUser(r), Via: "web"})
		if err != nil {
			log.Printf("web: plugin install %q: %v", ref, err)
			flash := "install-error"
			if errors.Is(err, plugin.ErrUnverifiedDisallowed) {
				flash = "install-unverified"
			}
			redirectIntegrations(w, r, "/integrations", flash)
			return
		}
		logAudit(d, r, "plugin.install", res.ID, "", res.Version)
		http.Redirect(w, r, "/integrations/"+url.PathEscape(res.ID), http.StatusSeeOther)
	}
}

func integrationsRefreshHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := pluginManager(d)
		if m == nil {
			redirectIntegrations(w, r, "/integrations", "refresh-error")
			return
		}
		if _, err := m.Catalog(true); err != nil && !errors.Is(err, plugin.ErrNoRegistryKeys) {
			log.Printf("web: plugin catalog refresh: %v", err)
			redirectIntegrations(w, r, "/integrations", "refresh-error")
			return
		}
		redirectIntegrations(w, r, "/integrations", "refreshed")
	}
}

func integrationsUpdateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			redirectIntegrations(w, r, "/integrations", "update-error")
			return
		}
		res, err := m.Update(plugin.UpdateRequest{ID: id, Actor: auditUser(r), Via: "web"})
		if err != nil {
			log.Printf("web: plugin update %q: %v", id, err)
			redirectIntegrations(w, r, "/integrations", "update-error")
			return
		}
		logAudit(d, r, "plugin.update", id, "", "")
		for _, u := range res {
			if len(u.NeedsConsent) > 0 {
				http.Redirect(w, r, "/integrations/"+url.PathEscape(id)+"/enable?pending=1", http.StatusSeeOther)
				return
			}
		}
		redirectIntegrations(w, r, "/integrations", "updated")
	}
}
