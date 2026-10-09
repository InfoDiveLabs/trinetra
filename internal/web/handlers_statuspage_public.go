package web

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
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

// stateLabel is a service state in words a customer understands.
var stateLabel = map[core.ServiceState]string{
	core.StateOperational: "Operational",
	core.StateDegraded:    "Degraded performance",
	core.StateOutage:      "Outage",
	core.StateMaintenance: "Under maintenance",
}

type publicServiceView struct {
	core.PublicService
	Label  string
	Uptime string // share of days with data that had no outage, "" with no data
}

type publicServiceGroup struct {
	Name     string
	Services []publicServiceView
}

type StatusPublicPageData struct {
	PublicPageData
	Status       core.PublicStatus
	OverallLabel string
	Summary      string
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
		out[i].Services = append(out[i].Services, publicServiceView{PublicService: s, Label: stateLabel[s.State], Uptime: uptimeText(s.History)})
	}
	return out
}

func uptimeText(days []core.PublicDay) string {
	var with, up int
	for _, d := range days {
		if d.State == "" {
			continue
		}
		with++
		if d.State != core.StateOutage {
			up++
		}
	}
	if with == 0 {
		return ""
	}
	if up == with {
		return "100%"
	}
	return fmt.Sprintf("%.2f%%", float64(up)*100/float64(with))
}

// statusSummary says in one line how many services are fine and what the
// rest are doing.
func statusSummary(svcs []core.PublicService) string {
	n := map[core.ServiceState]int{}
	for _, s := range svcs {
		n[s.State]++
	}
	total := len(svcs)
	if n[core.StateOperational] == total {
		if total == 1 {
			return "The service is working normally"
		}
		return fmt.Sprintf("All %d services are working normally", total)
	}
	parts := []string{fmt.Sprintf("%d of %d services working normally", n[core.StateOperational], total)}
	if c := n[core.StateOutage]; c > 0 {
		parts = append(parts, fmt.Sprintf("%d down", c))
	}
	if c := n[core.StateDegraded]; c > 0 {
		parts = append(parts, fmt.Sprintf("%d degraded", c))
	}
	if c := n[core.StateMaintenance]; c > 0 {
		parts = append(parts, fmt.Sprintf("%d under maintenance", c))
	}
	return strings.Join(parts, " · ")
}

func renderBareStatusPage(w http.ResponseWriter, page string, data any) {
	t, err := template.New("base_bare.html").Funcs(funcMap).ParseFS(templatesFS, "templates/base_bare.html", "templates/"+page)
	if err != nil {
		log.Printf("web: status page %s: parse: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base_bare.html", data); err != nil {
		log.Printf("web: status page %s: render: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// statusBaseURL picks the origin for absolute feed links: the configured
// web.origin, else the origin the request middleware derived (honours the
// package's proxy-header trust policy), else the request's own Host.
func statusBaseURL(d Deps, r *http.Request) string {
	if o := strings.TrimRight(d.Cfg().Web.Origin, "/"); o != "" {
		return o
	}
	if o := requestOriginFromContext(r); o != "" {
		return o
	}
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
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

type atomAuthor struct {
	Name string `xml:"name"`
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
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
	Author  atomAuthor  `xml:"author"`
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
		base := statusBaseURL(d, r)
		var entries []atomEntry
		for _, inc := range append(append([]core.PublicIncident{}, pub.Active...), pub.Recent...) {
			for _, u := range inc.Updates {
				entries = append(entries, atomEntry{
					Title:   fmt.Sprintf("%s \u2014 %s", inc.Title, capitalize(u.Status)),
					ID:      fmt.Sprintf("urn:trinetra:status:%s:%s", inc.ID, u.ID),
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
		feed := atomFeed{Title: pub.Title, ID: "urn:trinetra:status:feed", Author: atomAuthor{Name: pub.Title}, Updated: updated,
			Link: []atomLink{{Href: base + "/", Rel: "alternate"}, {Href: base + "/status/feed.atom", Rel: "self"}}, Entries: entries}
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(feed)
	}
}
