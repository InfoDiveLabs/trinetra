package web

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

var incidentStatuses = []string{core.IncidentInvestigating, core.IncidentIdentified, core.IncidentMonitoring, core.IncidentResolved}

type StatusIncidentsPageData struct {
	PageData
	Active, Resolved []core.StatusIncident
	Services         []core.StatusService
	Statuses         []string
	Unavailable      string
	Error            string
}

type StatusIncidentPageData struct {
	PageData
	Incident    core.StatusIncident
	ServiceName map[string]string
	Statuses    []string
	IsAdmin     bool
	Error       string
}

func buildIncidentsPage(r *http.Request, d Deps) StatusIncidentsPageData {
	data := StatusIncidentsPageData{PageData: newPageData(r, d, "Status updates", "Public incidents and what customers see"), Statuses: incidentStatuses}
	sp := statusPageAPI(d)
	if sp == nil {
		data.Unavailable = "The status page is not available from this daemon."
		return data
	}
	incs, err := sp.Incidents(true)
	if err != nil {
		data.Unavailable = statusUnavailableText(err)
		return data
	}
	for _, inc := range incs {
		if inc.Status == core.IncidentResolved {
			data.Resolved = append(data.Resolved, inc)
		} else {
			data.Active = append(data.Active, inc)
		}
	}
	data.Services, _ = sp.Services()
	return data
}

func statusIncidentsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderStatusTemplate(w, "statuspage_incidents.html", buildIncidentsPage(r, d), http.StatusOK)
	}
}

func statusIncidentCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		in := core.NewIncident{Title: r.FormValue("title"), Services: r.Form["services"], Impact: core.ServiceState(r.FormValue("impact")),
			Update: core.NewUpdate{Status: r.FormValue("status"), Message: r.FormValue("message")}}
		inc, err := sp.CreateIncident(in, auditUser(r))
		if errors.Is(err, core.ErrNotFound) {
			// No URL id here: the daemon's not-found means an unknown service id.
			err = fmt.Errorf("unknown service: %s", err)
		}
		if err != nil {
			data := buildIncidentsPage(r, d)
			data.Error = err.Error()
			renderStatusTemplate(w, "statuspage_incidents.html", data, http.StatusBadRequest)
			return
		}
		logAudit(d, r, "status_page.incident.create", inc.ID, "", inc.Title)
		http.Redirect(w, r, "/status-page/incidents/"+inc.ID, http.StatusSeeOther)
	}
}

func buildIncidentPage(r *http.Request, d Deps, id string) (StatusIncidentPageData, int) {
	data := StatusIncidentPageData{PageData: newPageData(r, d, "Status update", ""), Statuses: incidentStatuses,
		IsAdmin: currentRole(r) == string(RoleAdmin), ServiceName: map[string]string{}}
	sp := statusPageAPI(d)
	if sp == nil {
		return data, http.StatusNotFound
	}
	inc, err := sp.Incident(id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return data, http.StatusNotFound
		}
		data.Error = statusUnavailableText(err)
		return data, http.StatusOK
	}
	data.Incident = inc
	data.Sub = inc.Title
	svcs, _ := sp.Services()
	for _, s := range svcs {
		data.ServiceName[s.ID] = s.Name
	}
	return data, http.StatusOK
}

func statusIncidentPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, code := buildIncidentPage(r, d, r.PathValue("id"))
		if code == http.StatusNotFound {
			http.NotFound(w, r)
			return
		}
		renderStatusTemplate(w, "statuspage_incident.html", data, code)
	}
}

// incidentMutation runs op and redirects back to the incident, or re-renders
// it with the error (400) / 404.
func incidentMutation(d Deps, action string, op func(sp core.StatusPageAPI, r *http.Request, id string) (core.StatusIncident, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		inc, err := op(sp, r, id)
		if errors.Is(err, core.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			data, _ := buildIncidentPage(r, d, id)
			data.Error = err.Error()
			renderStatusTemplate(w, "statuspage_incident.html", data, http.StatusBadRequest)
			return
		}
		logAudit(d, r, action, id, "", inc.Status)
		http.Redirect(w, r, "/status-page/incidents/"+id, http.StatusSeeOther)
	}
}

func statusUpdatePostHandler(d Deps) http.HandlerFunc {
	return incidentMutation(d, "status_page.update.post", func(sp core.StatusPageAPI, r *http.Request, id string) (core.StatusIncident, error) {
		return sp.PostUpdate(id, core.NewUpdate{Status: r.FormValue("status"), Message: r.FormValue("message")}, auditUser(r))
	})
}

func statusUpdateEditHandler(d Deps) http.HandlerFunc {
	return incidentMutation(d, "status_page.update.edit", func(sp core.StatusPageAPI, r *http.Request, id string) (core.StatusIncident, error) {
		return sp.EditUpdate(id, r.PathValue("uid"), core.NewUpdate{Status: r.FormValue("status"), Message: r.FormValue("message")}, auditUser(r))
	})
}

func statusIncidentEditHandler(d Deps) http.HandlerFunc {
	return incidentMutation(d, "status_page.incident.edit", func(sp core.StatusPageAPI, r *http.Request, id string) (core.StatusIncident, error) {
		_ = r.ParseForm()
		// Resolve the URL id first so ErrNotFound from EditIncident can only
		// mean an unknown service in the body (400, not 404).
		if _, err := sp.Incident(id); err != nil {
			return core.StatusIncident{}, err
		}
		inc, err := sp.EditIncident(id, r.FormValue("title"), r.Form["services"], auditUser(r))
		if errors.Is(err, core.ErrNotFound) {
			err = fmt.Errorf("unknown service: %s", err)
		}
		return inc, err
	})
}

func statusIncidentDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		if err := sp.DeleteIncident(id, auditUser(r)); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logAudit(d, r, "status_page.incident.delete", id, "", "")
		http.Redirect(w, r, "/status-page/incidents", http.StatusSeeOther)
	}
}

// multiline renders plain text with line breaks AFTER escaping.
func multiline(s string) template.HTML {
	return template.HTML(strings.ReplaceAll(template.HTMLEscapeString(s), "\n", "<br>"))
}
