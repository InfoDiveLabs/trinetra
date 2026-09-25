package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// assetVersion is a short content hash over every embedded asset, appended as
// a ?v= query to asset URLs (see the "asset" template helper). Because the
// hash changes whenever any asset's bytes change, each build produces fresh
// asset URLs -- defeating stale browser/CDN (Cloudflare) caching of old JS/CSS
// that would otherwise persist for the CDN's edge-TTL. Computed once at
// startup; embed.FS reads are in-memory.
var assetVersion = computeAssetVersion()

func computeAssetVersion() string {
	h := sha256.New()
	_ = fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, rerr := assetsFS.ReadFile(p)
		if rerr != nil {
			return nil
		}
		h.Write([]byte(p))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// assetURL appends the content-hash version to an asset path so the template
// emits e.g. /assets/app.js?v=<hash>. Paired with the immutable Cache-Control
// the asset handler sets, this gives correct long-lived caching: unchanged
// assets stay cached forever, a changed asset gets a new URL.
func assetURL(path string) string { return path + "?v=" + assetVersion }

// templatesFS embeds internal/web/templates: base.html (the ported mockup
// shell -- nav/topbar/content blocks, see that file's comments) plus one file
// per page that fills in the "content" block (and, later, overrides other
// blocks as needed).
//
//go:embed templates
var templatesFS embed.FS

// funcMap holds the template helpers base.html and page templates call.
var funcMap = template.FuncMap{
	// ledClass/humanBytes/humanRate/diskTrendText/diskTrendClass/
	// loadLedClass/diskWarnPct/diskCriticalPct/subInt (handlers_dashboard.go)
	// are templates/dashboard.html's formatting helpers for the live
	// DashboardView.
	"ledClass":        ledClass,
	"humanBytes":      humanBytes,
	"humanRate":       humanRate,
	"diskTrendText":   diskTrendText,
	"diskTrendClass":  diskTrendClass,
	"loadLedClass":    loadLedClass,
	"diskWarnPct":     func() float64 { return DiskWarnPct },
	"diskCriticalPct": func() float64 { return DiskCriticalPct },
	"subInt":          subInt,
	// memBarPct (handlers_monitoring.go) is templates/monitoring.html's
	// container/process memory-meter width normalizer.
	"memBarPct": memBarPct,
	// asset appends the build's content-hash to an asset path for cache-busting
	// (see assetVersion); templates reference assets via {{asset "/assets/x"}}.
	"asset": assetURL,
}

// NavItem is one entry in the sidebar nav -- either a section heading (just
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
// icons, labels, admin gating) with mockup .html paths swapped for the
// server's real routes. Badge values are deliberately NOT set here: the
// mockup's hardcoded demo counts (Monitoring 220 / Alerts 2 / Channels 5 /
// Users 3) are computed fresh per request instead (navCountsFor, badgeFor
// below) so they never go stale.
var navItems = []navEntry{
	{NavItem: NavItem{Heading: "Monitor"}},
	{NavItem: NavItem{Href: "/", Icon: "◉", Label: "Dashboard"}},
	{NavItem: NavItem{Href: "/monitoring", Icon: "▤", Label: "Monitoring"}},
	{NavItem: NavItem{Href: "/host", Icon: "▢", Label: "Host"}},
	{NavItem: NavItem{Href: "/alerts", Icon: "!", Label: "Alerts"}},
	{NavItem: NavItem{Href: "/history", Icon: "◔", Label: "History"}},
	{NavItem: NavItem{Heading: "Admin"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/config", Icon: "⚙", Label: "Configuration"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/channels", Icon: "✉", Label: "Channels"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/users", Icon: "◇", Label: "Users"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/settings/public", Icon: "◈", Label: "Public view"}, AdminOnly: true},
}

// navForRole returns navItems filtered to what role may see (viewers get
// everything except AdminOnly entries, admins get everything -- the
// server-side equivalent of the mockup app.js NAV.filter(role==='admin' ||
// !n.admin)) with each entry's Badge filled in from counts via badgeFor.
func navForRole(role string, counts NavCounts) []NavItem {
	out := make([]NavItem, 0, len(navItems))
	for _, n := range navItems {
		if n.AdminOnly && role != "admin" {
			continue
		}
		item := n.NavItem
		item.Badge = badgeFor(item.Href, counts)
		out = append(out, item)
	}
	return out
}

// badgeFor maps a nav entry's Href to the NavCounts field it displays,
// rendered through badgeText (nav_counts.go) so a zero/unknown count is an
// empty string (no badge) rather than a stale "0". Hrefs with no counter
// (Dashboard, History, Configuration, Public view) and section headings
// (empty Href) fall through to "" harmlessly.
func badgeFor(href string, counts NavCounts) string {
	switch href {
	case "/monitoring":
		return badgeText(counts.Monitoring)
	case "/alerts":
		return badgeText(counts.Alerts)
	case "/channels":
		return badgeText(counts.Channels)
	case "/users":
		return badgeText(counts.Users)
	default:
		return ""
	}
}

// currentRole returns the signed-in request's role ("admin"/"viewer"), or
// "" for an anonymous one. userMiddleware (middleware.go) is what actually
// resolves the session into a *User this reads back via userFromContext;
// this is purely the cosmetic input to nav filtering (navForRole) and the
// topbar/sidebar role badge -- access control itself is requireRole's job,
// not this function's.
func currentRole(r *http.Request) string {
	if u, ok := userFromContext(r); ok {
		return string(u.Role)
	}
	return ""
}

// PageData is what every page template renders against: base.html's shell
// (nav/topbar) plus whatever the page itself needs.
type PageData struct {
	// Title/Sub drive the topbar's <h1>/<p>, mirroring the mockup's
	// data-title/data-sub attributes.
	Title, Sub string
	// ServerName is this host's display name (config server.name, or the
	// hostname when unset), shown as the sidebar brand subtitle so a multi-host
	// operator can tell which host's panel they are looking at (#101). Replaces
	// the old hardcoded MONITOR.HOME.LAN.
	ServerName string
	// Status/StatusText drive the topbar's status pill (led color class +
	// display text) and are ALWAYS computed by newPageData from the real
	// active-alert set (topbarStatus, over the control socket) -- see that function's
	// doc. They are not caller-supplied: every page's topbar reflects the
	// same real severity/counts rather than each page guessing its own
	// (the old bug this replaces: most page handlers passed the literal
	// "ok" into newPageData regardless of what was actually firing, and the
	// old statusText helper mapped "crit"/"warn" to hardcoded text like "2
	// alerts firing" no matter the real count).
	Status, StatusText string
	// Role is the current user's role ("admin" or "viewer"), from
	// currentRole. Drives both nav filtering and the read-only pill/footer.
	Role string
	// Name/Initial are the signed-in user's display name (User.Name) and its
	// uppercased first letter, rendered in the sidebar footer's identity block
	// (#80). Empty for an anonymous request. Previously the footer showed a
	// hardcoded name keyed only on Role ("Suraj"/"Aditi"), which misidentified
	// every user; these carry the real value from userFromContext.
	Name, Initial string
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
	// CSRF is the current session's anti-CSRF token (requireCSRF,
	// middleware.go), rendered into base.html's csrf-token meta tag so
	// app.js can read it into the X-CSRF-Token header of a mutating
	// fetch()/htmx request (e.g. the topbar's sign-out button). Empty when
	// there's no signed-in session in the request context.
	CSRF string
	// CoreVersion is the running core daemon's version (fetched over the
	// control socket), WebVersion is this web plugin's own compiled-in version,
	// and VersionMismatch is true when they differ -- so a partial upgrade
	// (plugin older than core, or vice versa) is legible in the sidebar footer
	// rather than silent (#107).
	CoreVersion     string
	WebVersion      string
	VersionMismatch bool
}

// newPageData builds the PageData every page handler needs, deriving Role
// from the request and Active from its path, and Status/StatusText from the
// real active-alert set (topbarStatus(activeAlertsViaAPI(d))) --
// see PageData's doc for why every page shares this one computation rather
// than each supplying its own. d is also used to compute the nav's live
// badge counts (navCountsFor); every other field is unchanged from the
// request/session.
func newPageData(r *http.Request, d Deps, title, sub string) PageData {
	role := currentRole(r)
	name := ""
	if u, ok := userFromContext(r); ok {
		name = u.Name
	}
	csrf := ""
	if sess, ok := sessionFromContext(r); ok {
		csrf = sess.CSRF
	}
	status, statusText := topbarStatus(activeAlertsViaAPI(r, d))
	webVer := version.String()
	coreVer := coreVersionViaAPI(r, d)
	return PageData{
		Title:           title,
		Sub:             sub,
		ServerName:      d.Cfg().ServerName(),
		Status:          status,
		StatusText:      statusText,
		Role:            role,
		Name:            name,
		Initial:         firstInitial(name),
		Active:          r.URL.Path,
		Nav:             navForRole(role, navCountsFor(r, d)),
		Nonce:           nonceFromContext(r),
		CSRF:            csrf,
		CoreVersion:     coreVer,
		WebVersion:      webVer,
		VersionMismatch: coreVer != "" && coreVer != "unknown" && coreVer != webVer,
	}
}

// coreVersionViaAPI fetches the running core daemon's version over the control
// socket (#107), degrading to "unknown" when the API is unset or errors so a
// version hiccup never breaks page rendering. Reads through apiFor(r, d)
// (node_scope.go), so a page scoped to a remote fleet node shows that node's
// own reported version rather than the master's.
func coreVersionViaAPI(r *http.Request, d Deps) string {
	api := apiFor(r, d)
	if api == nil {
		return "unknown"
	}
	v, err := api.Version()
	if err != nil || v == "" {
		return "unknown"
	}
	return v
}

// firstInitial returns the uppercased first rune of name (for the sidebar
// avatar), or "" for an empty name.
func firstInitial(name string) string {
	for _, r := range name {
		return strings.ToUpper(string(r))
	}
	return ""
}

// renderPageStatus parses base.html together with the named page template
// (whose {{define "content"}} overrides base.html's content block -- the
// standard html/template nested-layout pattern) and executes "base.html"
// against data, writing status before the body. Parsing per-request keeps
// each page's template set isolated (two pages both defining "content" in the
// same set would conflict), which is cheap enough here: embed.FS reads are
// in-memory and traffic is low; a future task can cache per-page
// *template.Template if this shows up in profiling. Handlers rendering a
// normal 200 page call it with http.StatusOK; the 403 denied panel
// (middleware.go's renderDenied) passes http.StatusForbidden.
func renderPageStatus(w http.ResponseWriter, page string, data PageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/"+page)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderDenied renders the mockup's "Admin only" denied panel (templates/
// denied.html, ported from ui-mockup/assets/app.js's `.panel.denied` markup)
// through the full app-shell layout with a 403 status -- requireRole
// (middleware.go) calls this when a signed-in user's role falls short of a
// route's required minimum. It still renders through the normal PageData/
// nav (the visitor IS signed in, so the shell should look like it does
// everywhere else), just with the content block replaced.
func renderDenied(w http.ResponseWriter, r *http.Request, d Deps) {
	data := newPageData(r, d, "Admin only", "Access denied")
	if err := renderPageStatus(w, "denied.html", data, http.StatusForbidden); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderNotFound is renderDenied's 404 counterpart (templates/notfound.html,
// same "panel denied" styling), through the full app-shell layout. reason is
// a short, user-facing explanation rendered into the panel body (e.g. "no
// such node") -- withNodeRouter (node_scope.go) is this task's caller, for
// an unresolvable or rejected /n/{node}/... path.
func renderNotFound(w http.ResponseWriter, r *http.Request, d Deps, reason string) {
	data := newPageData(r, d, "Not found", reason)
	if err := renderPageStatus(w, "notfound.html", data, http.StatusNotFound); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// BarePageData is what a "bare"/centered page (enroll now; login and the
// public dashboard in later tasks, per the mockup's login.html/public.html
// which use the same centered `.card`/`.center` styling rather than the app
// shell) renders against: just the title and the CSP nonce its boot script
// needs, none of PageData's nav/topbar/role fields -- those pages render
// before there's a signed-in session (or, for /public, deliberately without
// one) so the sidebar/topbar shell has nothing to fill in.
type BarePageData struct {
	// Title feeds the <title> tag, same as PageData.Title.
	Title string
	// Nonce is this request's CSP nonce (see security.go), threaded onto
	// base_bare.html's boot script tag exactly like PageData.Nonce.
	Nonce string
	// EnrollToken is this request's ?token= query parameter, if any,
	// threaded onto enroll.html's hidden #enrollToken field so app.js can
	// echo it back as /enroll/begin's "token" field (resolveEnrollRole,
	// enroll_tokens.go, is what actually validates/consumes it -- this is
	// just carrying the value from the GET's URL to the POST's body).
	// login.html doesn't reference this field; harmless there either way.
	EnrollToken string
}

// newBarePageData builds the BarePageData a bare-layout page handler needs.
func newBarePageData(r *http.Request, title string) BarePageData {
	return BarePageData{
		Title:       title,
		Nonce:       nonceFromContext(r),
		EnrollToken: r.URL.Query().Get("token"),
	}
}

// renderBarePage is renderPageStatus's counterpart for the bare/centered
// layout: it parses base_bare.html together with the named page template
// instead of base.html. See BarePageData's doc for why a page needs this
// instead of renderPageStatus.
func renderBarePage(w http.ResponseWriter, page string, data BarePageData) error {
	tmpl, err := template.New("base_bare.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base_bare.html", "templates/"+page)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base_bare.html", data)
}
