//go:build web

package web

import (
	"embed"
	"html/template"
	"net/http"
)

// templatesFS embeds internal/web/templates: base.html (the ported mockup
// shell — nav/topbar/content blocks, see that file's comments) plus one file
// per page that fills in the "content" block (and, later, overrides other
// blocks as needed).
//
//go:embed templates
var templatesFS embed.FS

// funcMap holds the template helpers base.html and page templates call.
var funcMap = template.FuncMap{
	// statusText mirrors the mockup app.js's stTxt map (the topbar's status
	// pill), keyed by the same ok/warn/crit status strings.
	"statusText": statusText,
}

// statusText maps a topbar status ("ok"/"warn"/"crit") to its display text.
// Anything else (including "") falls back to the "ok" text rather than
// rendering blank.
func statusText(status string) string {
	switch status {
	case "warn":
		return "1 warning"
	case "crit":
		return "2 alerts firing"
	default:
		return "All systems normal"
	}
}

// NavItem is one entry in the sidebar nav — either a section heading (just
// Heading set) or a link (Href/Icon/Label, optionally Badge), mirroring the
// mockup app.js NAV array's {h:...} and {p:,ic:,t:,ct:} shapes.
type NavItem struct {
	Heading string
	Href    string
	Icon    string
	Label   string
	Badge   string
}

// navEntry is a NavItem plus the role gate the mockup expressed as NAV's
// `admin:true` flag; navForRole filters on it and never exposes it to
// templates.
type navEntry struct {
	NavItem
	AdminOnly bool
}

// navItems mirrors the mockup app.js NAV array verbatim (headings, paths,
// icons, labels, badge counts, admin gating) with mockup .html paths
// swapped for the server's real routes. Later tasks implement the routes
// these link to (monitoring, alerts, history, config, channels, users,
// public-settings); for now they render as plain links even before their
// handlers exist.
var navItems = []navEntry{
	{NavItem: NavItem{Heading: "Monitor"}},
	{NavItem: NavItem{Href: "/", Icon: "◉", Label: "Dashboard"}},
	{NavItem: NavItem{Href: "/monitoring", Icon: "▤", Label: "Monitoring", Badge: "220"}},
	{NavItem: NavItem{Href: "/alerts", Icon: "!", Label: "Alerts", Badge: "2"}},
	{NavItem: NavItem{Href: "/history", Icon: "◔", Label: "History"}},
	{NavItem: NavItem{Heading: "Admin"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/config", Icon: "⚙", Label: "Configuration"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/channels", Icon: "✉", Label: "Channels", Badge: "5"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/users", Icon: "◇", Label: "Users", Badge: "3"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/settings/public", Icon: "◈", Label: "Public view"}, AdminOnly: true},
}

// navForRole returns navItems filtered to what role may see: viewers get
// everything except AdminOnly entries, admins get everything. This is the
// server-side equivalent of the mockup app.js NAV.filter(role==='admin' ||
// !n.admin).
func navForRole(role string) []NavItem {
	out := make([]NavItem, 0, len(navItems))
	for _, n := range navItems {
		if n.AdminOnly && role != "admin" {
			continue
		}
		out = append(out, n.NavItem)
	}
	return out
}

// currentRole is a stub: every request is treated as admin until Task 5/6
// add real sessions/RBAC (see the design doc's Security checklist and
// Pages/routes tables). Kept as a single seam so those tasks only need to
// change this one function's body.
func currentRole(r *http.Request) string {
	return "admin"
}

// PageData is what every page template renders against: base.html's shell
// (nav/topbar) plus whatever the page itself needs.
type PageData struct {
	// Title/Sub/Status drive the topbar (<h1>/<p>/status pill), mirroring
	// the mockup's data-title/data-sub/data-status attributes.
	Title, Sub, Status string
	// Role is the current user's role ("admin" or "viewer"), from
	// currentRole. Drives both nav filtering and the read-only pill/footer.
	Role string
	// Active is the request path, used to mark the matching nav link
	// class="active" (mirrors the mockup's here===n.p comparison).
	Active string
	// Nav is Role's filtered nav list, precomputed so the template doesn't
	// need role-aware logic beyond the active-link comparison.
	Nav []NavItem
	// Nonce is this request's per-response CSP nonce (see security.go's
	// securityHeaders/nonceFromContext), rendered onto the single htmx boot
	// script tag in base.html so it's authorized under the CSP's
	// script-src 'nonce-...' directive.
	Nonce string
}

// newPageData builds the PageData every page handler needs, deriving Role
// from the request and Active from its path.
func newPageData(r *http.Request, title, sub, status string) PageData {
	role := currentRole(r)
	return PageData{
		Title:  title,
		Sub:    sub,
		Status: status,
		Role:   role,
		Active: r.URL.Path,
		Nav:    navForRole(role),
		Nonce:  nonceFromContext(r),
	}
}

// renderPage parses base.html together with the named page template (whose
// {{define "content"}} overrides base.html's content block — the standard
// html/template nested-layout pattern) and executes "base.html" against
// data. Parsing per-request keeps each page's template set isolated (two
// pages both defining "content" in the same set would conflict), which is
// cheap enough here: embed.FS reads are in-memory and Task 2's traffic is
// low; a future task can cache per-page *template.Template if this shows up
// in profiling.
func renderPage(w http.ResponseWriter, page string, data PageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// BarePageData is what a "bare"/centered page (enroll now; login and the
// public dashboard in later tasks, per the mockup's login.html/public.html
// which use the same centered `.card`/`.center` styling rather than the app
// shell) renders against: just the title and the CSP nonce its boot script
// needs, none of PageData's nav/topbar/role fields — those pages render
// before there's a signed-in session (or, for /public, deliberately without
// one) so the sidebar/topbar shell has nothing to fill in.
type BarePageData struct {
	// Title feeds the <title> tag, same as PageData.Title.
	Title string
	// Nonce is this request's CSP nonce (see security.go), threaded onto
	// base_bare.html's boot script tag exactly like PageData.Nonce.
	Nonce string
}

// newBarePageData builds the BarePageData a bare-layout page handler needs.
func newBarePageData(r *http.Request, title string) BarePageData {
	return BarePageData{Title: title, Nonce: nonceFromContext(r)}
}

// renderBarePage is renderPage's counterpart for the bare/centered layout:
// it parses base_bare.html together with the named page template instead of
// base.html. See BarePageData's doc for why a page needs this instead of
// renderPage.
func renderBarePage(w http.ResponseWriter, page string, data BarePageData) error {
	tmpl, err := template.New("base_bare.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base_bare.html", "templates/"+page)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base_bare.html", data)
}
