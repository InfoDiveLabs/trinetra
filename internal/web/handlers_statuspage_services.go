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
	Default     int
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
	evals, _ := sp.Evaluation()
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
		fail := func(msg string) {
			data := buildStatusServicesPage(r, d)
			data.Error = msg
			renderStatusTemplate(w, "statuspage_services.html", data, http.StatusBadRequest)
		}
		hold, err := strconv.Atoi(strings.TrimSpace(r.FormValue("hold")))
		if err != nil {
			fail("hold-down must be a number of seconds")
			return
		}
		order, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("order")))
		targets, err := parseStatusTargetText(r.FormValue("targets"))
		if err != nil {
			fail(err.Error())
			return
		}
		svc := core.StatusService{ID: strings.TrimSpace(r.FormValue("id")), Name: r.FormValue("name"), Group: r.FormValue("group"),
			Description: r.FormValue("description"), Order: order, HoldDownSec: hold, Targets: targets}
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
