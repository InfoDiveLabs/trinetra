package web

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func statusPageAPI(d Deps) core.StatusPageAPI {
	if d.StatusPage == nil {
		return nil
	}
	return d.StatusPage()
}

// renderStatusTemplate renders the named status-page template (a file under
// templates/) inside base.html. Shared by the status-page admin pages.
func renderStatusTemplate(w http.ResponseWriter, page string, data any, status int) {
	t, err := template.New("base.html").Funcs(funcMap).ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = t.ExecuteTemplate(w, "base.html", data)
}

type statusServiceRow struct {
	core.StatusService
	Eval        core.ServiceEvaluation
	TargetsText string
}

type StatusServicesPageData struct {
	PageData
	Rows        []statusServiceRow
	Unavailable string // non-empty: show this notice instead of the editor
	Error       string
	Notice      string // non-fatal notice shown above the table
	Default     int
	Form        StatusServiceForm
}

// StatusServiceForm is the add/edit form's field values (sticky on error,
// prefilled on edit).
type StatusServiceForm struct {
	ID, Name, Group, Order, Hold, Description, Targets string
	Edit                                               bool
}

func formatStatusTarget(t core.StatusTarget) string {
	switch t.Kind {
	case core.TargetHost:
		return "host"
	case core.TargetNode:
		return "node:" + t.Node
	}
	s := t.Kind + ":" + t.Value
	if t.Node != "" {
		s += "@" + t.Node
	}
	return s
}

// parseStatusTargetText parses one target per line (same grammar as the CLI).
func parseStatusTargetText(text string) ([]core.StatusTarget, error) {
	var out []core.StatusTarget
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "host" {
			out = append(out, core.StatusTarget{Kind: core.TargetHost})
			continue
		}
		kind, rest, ok := strings.Cut(line, ":")
		if !ok || rest == "" {
			return nil, errors.New("bad target " + strconv.Quote(line))
		}
		if kind == core.TargetNode {
			out = append(out, core.StatusTarget{Kind: kind, Node: rest})
			continue
		}
		val, node, _ := strings.Cut(rest, "@")
		out = append(out, core.StatusTarget{Kind: kind, Value: val, Node: node})
	}
	return out, nil
}

func statusUnavailableText(err error) string {
	if errors.Is(err, core.ErrStatusPageOnChild) {
		return "The public status page runs on the fleet master. Manage it there."
	}
	return "The status page is not available from this daemon: " + err.Error()
}

func buildStatusServicesPage(r *http.Request, d Deps) StatusServicesPageData {
	data := StatusServicesPageData{PageData: newPageData(r, d, "Status page", "Public services and their live state"), Default: core.DefaultHoldDownSec}
	data.Form = StatusServiceForm{Order: "0", Hold: strconv.Itoa(core.DefaultHoldDownSec)}
	sp := statusPageAPI(d)
	if sp == nil {
		data.Unavailable = "The status page is not available from this daemon."
		return data
	}
	svcs, err := sp.Services()
	if err != nil {
		data.Unavailable = statusUnavailableText(err)
		return data
	}
	evals, evalErr := sp.Evaluation()
	if evalErr != nil {
		data.Notice = "Live status unavailable: " + evalErr.Error()
	}
	byID := map[string]core.ServiceEvaluation{}
	for _, e := range evals {
		byID[e.ServiceID] = e
	}
	for _, s := range svcs {
		lines := make([]string, len(s.Targets))
		for i, t := range s.Targets {
			lines[i] = formatStatusTarget(t)
		}
		data.Rows = append(data.Rows, statusServiceRow{StatusService: s, Eval: byID[s.ID], TargetsText: strings.Join(lines, "\n")})
		if id := r.URL.Query().Get("edit"); id != "" && id == s.ID && r.Method == http.MethodGet {
			data.Form = StatusServiceForm{ID: s.ID, Name: s.Name, Group: s.Group, Order: strconv.Itoa(s.Order),
				Hold: strconv.Itoa(s.HoldDownSec), Description: s.Description, Targets: strings.Join(lines, "\n"), Edit: true}
		}
	}
	return data
}

func statusServicesPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderStatusTemplate(w, "statuspage_services.html", buildStatusServicesPage(r, d), http.StatusOK)
	}
}

func statusServiceSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		form := StatusServiceForm{ID: strings.TrimSpace(r.FormValue("id")), Name: strings.TrimSpace(r.FormValue("name")),
			Group: strings.TrimSpace(r.FormValue("group")), Order: strings.TrimSpace(r.FormValue("order")),
			Hold: strings.TrimSpace(r.FormValue("hold")), Description: strings.TrimSpace(r.FormValue("description")),
			Targets: r.FormValue("targets")}
		fail := func(msg string) {
			data := buildStatusServicesPage(r, d)
			data.Form = form
			data.Form.Edit = r.FormValue("edit") == "1"
			data.Error = msg
			renderStatusTemplate(w, "statuspage_services.html", data, http.StatusBadRequest)
		}
		hold, err := strconv.Atoi(form.Hold)
		if err != nil {
			fail("hold-down must be a number of seconds")
			return
		}
		order := 0
		if form.Order != "" {
			if order, err = strconv.Atoi(form.Order); err != nil {
				fail("order must be a whole number")
				return
			}
		}
		targets, err := parseStatusTargetText(r.FormValue("targets"))
		if err != nil {
			fail(err.Error())
			return
		}
		editing := r.FormValue("edit") == "1"
		existing, err := sp.Services()
		if err != nil {
			fail(err.Error())
			return
		}
		exists := false
		for _, e := range existing {
			if e.ID == form.ID {
				exists = true
			}
		}
		switch {
		case !editing && exists:
			fail("a service with id " + form.ID + " already exists \u2014 use Edit")
			return
		case editing && !exists:
			fail("no such service")
			return
		}
		svc := core.StatusService{ID: form.ID, Name: form.Name, Group: form.Group,
			Description: form.Description, Order: order, HoldDownSec: hold, Targets: targets}
		saved, err := sp.SetService(svc, auditUser(r))
		if err != nil {
			fail(err.Error())
			return
		}
		logAudit(d, r, "status_page.service.save", saved.ID, "", saved.Name)
		http.Redirect(w, r, "/status-page/services", http.StatusSeeOther)
	}
}

func statusServiceDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sp := statusPageAPI(d)
		if sp == nil {
			http.NotFound(w, r)
			return
		}
		id := r.PathValue("id")
		if err := sp.DeleteService(id, auditUser(r)); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		logAudit(d, r, "status_page.service.delete", id, "", "")
		http.Redirect(w, r, "/status-page/services", http.StatusSeeOther)
	}
}
