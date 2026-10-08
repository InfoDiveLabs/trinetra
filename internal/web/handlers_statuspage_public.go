package web

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// publicStatusData returns the sanitized public status, or ok=false when the
// status page isn't set up (no API, an error, or zero services).
func publicStatusData(d Deps) (core.PublicStatus, bool) {
	sp := statusPageAPI(d)
	if sp == nil {
		return core.PublicStatus{}, false
	}
	pub, err := sp.Public()
	if err != nil || len(pub.Services) == 0 {
		return core.PublicStatus{}, false
	}
	return pub, true
}

var overallLabel = map[string]string{
	core.OverallOperational: "All systems operational",
	core.OverallPartial:     "Partial outage",
	core.OverallMajor:       "Major outage",
	core.OverallMaintenance: "Under maintenance",
}

type publicServiceGroup struct {
	Name     string
	Services []core.PublicService
}

type StatusPublicPageData struct {
	PublicPageData
	Status       core.PublicStatus
	OverallLabel string
	Groups       []publicServiceGroup
}

func groupPublicServices(svcs []core.PublicService) []publicServiceGroup {
	var out []publicServiceGroup
	idx := map[string]int{}
	for _, s := range svcs {
		i, ok := idx[s.Group]
		if !ok {
			i = len(out)
			idx[s.Group] = i
			out = append(out, publicServiceGroup{Name: s.Group})
		}
		out[i].Services = append(out[i].Services, s)
	}
	return out
}

func renderBareStatusPage(w http.ResponseWriter, page string, data any) {
	t, err := template.New("base_bare.html").Funcs(funcMap).ParseFS(templatesFS, "templates/base_bare.html", "templates/"+page)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = t.ExecuteTemplate(w, "base_bare.html", data)
}

func publicGate(d Deps, w http.ResponseWriter, r *http.Request) bool {
	if !d.Cfg().Public.Enabled {
		http.NotFound(w, r)
		return false
	}
	return true
}

func statusHistoryHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !publicGate(d, w, r) {
			return
		}
		pub, ok := publicStatusData(d)
		if !ok {
			http.NotFound(w, r)
			return
		}
		renderBareStatusPage(w, "status_history.html", StatusPublicPageData{
			PublicPageData: PublicPageData{BarePageData: newBarePageData(r, pub.Title+" history")},
			Status:         pub, OverallLabel: overallLabel[pub.Overall],
		})
	}
}

func statusAPIHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !publicGate(d, w, r) {
			return
		}
		pub, ok := publicStatusData(d)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Cache-Control", "public, max-age=30")
		_ = json.NewEncoder(w).Encode(pub)
	}
}

type atomText struct {
	Type string `xml:"type,attr,omitempty"`
	Body string `xml:",chardata"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr,omitempty"`
}

type atomEntry struct {
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Updated string   `xml:"updated"`
	Link    atomLink `xml:"link"`
	Content atomText `xml:"content"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Title   string      `xml:"title"`
	ID      string      `xml:"id"`
	Updated string      `xml:"updated"`
	Link    []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

func statusFeedHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !publicGate(d, w, r) {
			return
		}
		pub, ok := publicStatusData(d)
		if !ok {
			http.NotFound(w, r)
			return
		}
		base := "https://" + r.Host
		if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
			base = "http://" + r.Host
		}
		var entries []atomEntry
		for _, inc := range append(append([]core.PublicIncident{}, pub.Active...), pub.Recent...) {
			for _, u := range inc.Updates {
				entries = append(entries, atomEntry{
					Title:   fmt.Sprintf("%s — %s", inc.Title, u.Status),
					ID:      fmt.Sprintf("%s/status/incident/%s/%d", base, inc.ID, u.TS),
					Updated: time.Unix(u.TS, 0).UTC().Format(time.RFC3339),
					Link:    atomLink{Href: base + "/status/history#" + inc.ID},
					Content: atomText{Type: "text", Body: u.Message},
				})
			}
		}
		sort.SliceStable(entries, func(i, j int) bool { return entries[i].Updated > entries[j].Updated })
		if len(entries) > 50 {
			entries = entries[:50]
		}
		updated := time.Unix(pub.Generated, 0).UTC().Format(time.RFC3339)
		if len(entries) > 0 {
			updated = entries[0].Updated
		}
		feed := atomFeed{Title: pub.Title, ID: base + "/status/feed.atom", Updated: updated,
			Link: []atomLink{{Href: base + "/", Rel: "alternate"}, {Href: base + "/status/feed.atom", Rel: "self"}}, Entries: entries}
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(feed)
	}
}
