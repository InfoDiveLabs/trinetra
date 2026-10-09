package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/version"
)

// assetVersion is a short content hash over every embedded asset, appended as a ?v= query
// to asset URLs (see the "asset" template helper).
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

// assetURL appends the content-hash version to an asset path so the template emits e.g.
// /assets/app.js?v=<hash>.
func assetURL(path string) string { return path + "?v=" + assetVersion }

// templatesFS embeds the page templates; base.html is the app shell.
//
//go:embed templates
var templatesFS embed.FS

// funcMap holds the template helpers base.html and page templates call.
var funcMap = template.FuncMap{
	// Formatting helpers for the dashboard's live view.
	"ledClass": ledClass,
	// multiline/timeText (handlers_statuspage_incidents.go, handlers_fleet.go)
	// render status-page update text and unix timestamps.
	"multiline":       multiline,
	"timeText":        incidentTimeText,
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
	// nodeHref/nodeAgo/nodeDur/clockTime/linkUnreachable back every same-origin link's node
	// prefix and the replica banner / child link badge's time and threshold formatting.
	"nodeHref":        nodeHref,
	"nodeAgo":         nodeAgoText,
	"nodeDur":         nodeDurText,
	"clockTime":       nodeClockTime,
	"linkUnreachable": linkUnreachable,
	// oneIndexed (U6, 2026-09-25 UI audit fix) renders a zero-based loop index as its 1-based
	// display label -- fleet_alerting.html's Route/ Policy/Step row headings only.
	"oneIndexed": oneIndexed,
	// dict builds a map[string]any from alternating key/value arguments, for passing a small
	// ad-hoc bundle of fields into a named template block.
	"dict": templateDict,
	// managedKeyKnown reports whether key is one of core.ManagedKeys -- the create/edit form's
	// key <select> uses it to add a visible.
	"managedKeyKnown": func(key string) bool {
		for _, k := range core.ManagedKeys {
			if k == key {
				return true
			}
		}
		return false
	},
}

// nodeHref joins a node scope's URL prefix (nodeScope.Prefix, node_scope.go: "" for self,
// "/n/<id>" for a remote node) with a same-origin route path.
func nodeHref(prefix, path string) string {
	return prefix + path
}

// nodeDurText renders a Unix timestamp as a short duration since now, with no "ago" suffix:
// "3s", "2m", "1h", "4d".
func nodeDurText(ts int64) string {
	if ts <= 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0))
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// nodeAgoText is nodeDurText with an " ago" suffix ("3s ago", "2m ago"), except ts<=0 which
// stays the bare "never" (an "never ago" reading would be wrong).
func nodeAgoText(ts int64) string {
	d := nodeDurText(ts)
	if ts <= 0 {
		return d
	}
	return d + " ago"
}

// nodeClockTime renders a Unix timestamp as a local HH:MM clock reading, for the replica
// banner's "child1 is down since 14:02" text. ts<=0 renders "unknown".
func nodeClockTime(ts int64) string {
	if ts <= 0 {
		return "unknown"
	}
	return time.Unix(ts, 0).Local().Format("15:04")
}

// linkUnreachableThreshold is the cutoff for the topbar child-link pill.
const linkUnreachableThreshold = 2 * time.Minute

// linkUnreachable reports whether a child's link to its master (from Fleet().Status().Link,
// core.LinkView) has been retrying for longer than linkUnreachableThreshold.
func linkUnreachable(l *core.LinkView) bool {
	if l == nil || l.State != "retrying" {
		return false
	}
	if l.LastAck <= 0 {
		return true
	}
	return time.Since(time.Unix(l.LastAck, 0)) > linkUnreachableThreshold
}

// NavItem is one sidebar entry: a section heading (Heading only) or a link.
type NavItem struct {
	Heading string
	Href    string
	Icon    string // a navIcons name
	Label   string
	Badge   string
}

// navEntry adds the gates navForRole filters on.
type navEntry struct {
	NavItem
	AdminOnly bool
	MinRole   Role // minimum role rank (zero = viewer)
	// MasterOnly entries show only on a fleet master and are master-local
	// URLs, so they are never node-prefixed.
	MasterOnly bool
}

// navItems is the sidebar.
var navItems = []navEntry{
	{NavItem: NavItem{Heading: "Fleet"}},
	{NavItem: NavItem{Href: "/fleet", Icon: "fleet", Label: "Fleet"}, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/incidents", Icon: "incidents", Label: "Incidents"}, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/silences", Icon: "silence", Label: "Silences"}, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/alerting", Icon: "zap", Label: "Alerting"}, AdminOnly: true, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/admin", Icon: "shield", Label: "Fleet admin"}, AdminOnly: true, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/managed", Icon: "file", Label: "Managed config"}, AdminOnly: true, MasterOnly: true},
	{NavItem: NavItem{Href: "/fleet/audit", Icon: "list", Label: "Audit"}, AdminOnly: true, MasterOnly: true},
	{NavItem: NavItem{Heading: "Monitor"}},
	{NavItem: NavItem{Href: "/", Icon: "dashboard", Label: "Dashboard"}},
	{NavItem: NavItem{Href: "/monitoring", Icon: "pulse", Label: "Monitoring"}},
	{NavItem: NavItem{Href: "/host", Icon: "host", Label: "Host"}},
	{NavItem: NavItem{Href: "/alerts", Icon: "bell", Label: "Alerts"}},
	{NavItem: NavItem{Href: "/history", Icon: "history", Label: "History"}},
	{NavItem: NavItem{Heading: "Status page"}},
	{NavItem: NavItem{Href: "/status-page/incidents", Icon: "announce", Label: "Status updates"}, MinRole: RoleResponder},
	{NavItem: NavItem{Href: "/status-page/services", Icon: "status", Label: "Services"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/settings/public", Icon: "globe", Label: "Public view"}, AdminOnly: true},
	{NavItem: NavItem{Heading: "Settings"}},
	{NavItem: NavItem{Href: "/config", Icon: "sliders", Label: "Server settings"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/channels", Icon: "send", Label: "Notifications"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/users", Icon: "users", Label: "Users"}, AdminOnly: true},
	{NavItem: NavItem{Href: "/updates", Icon: "update", Label: "Updates"}, AdminOnly: true},
}

// navForRole returns navItems filtered to what role may see.
func navForRole(role string, counts NavCounts, node nodeScope, fleetRole string) []NavItem {
	out := make([]NavItem, 0, len(navItems))
	for _, n := range navItems {
		if n.AdminOnly && (role != "admin" || !node.Self) {
			continue
		}
		if n.MinRole != "" && roleRank(Role(role)) < roleRank(n.MinRole) {
			continue
		}
		if n.MasterOnly && fleetRole != config.RoleMaster {
			continue
		}
		item := n.NavItem
		if item.Heading != "" {
			out = append(out, item)
			continue
		}
		item.Badge = badgeFor(item.Href, counts)
		if item.Href != "" && !n.MasterOnly {
			item.Href = nodeHref(node.Prefix, item.Href)
		}
		out = append(out, item)
	}
	return dropEmptyHeadings(out)
}

// dropEmptyHeadings removes headings with no visible item under them.
func dropEmptyHeadings(items []NavItem) []NavItem {
	out := items[:0]
	for i, it := range items {
		if it.Heading != "" && (i+1 == len(items) || items[i+1].Heading != "") {
			continue
		}
		out = append(out, it)
	}
	return out
}

// badgeFor maps a nav entry's Href to the NavCounts field it displays, rendered through
// badgeText (nav_counts.go) so a zero/unknown count is an empty string.
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
	case "/fleet":
		return badgeText(counts.FleetDown)
	case "/fleet/incidents":
		return badgeText(counts.IncidentsFiring)
	default:
		return ""
	}
}

// currentRole returns the signed-in request's role ("admin"/"responder"/"viewer"), or ""
// for an anonymous one. userMiddleware.
func currentRole(r *http.Request) string {
	if u, ok := userFromContext(r); ok {
		return string(u.Role)
	}
	return ""
}

// PageData is what every page template renders against: base.html's shell
// (nav/topbar) plus whatever the page itself needs.
type PageData struct {
	// Title/Sub drive the topbar's <h1>/<p>.
	Title, Sub string
	// ServerName is this host's display name (config server.name, or the hostname when unset).
	ServerName string
	// Status/StatusText drive the topbar's status pill (led color class + display text) and
	// are ALWAYS computed by newPageData from the real active-alert set.
	Status, StatusText string
	// Role is the current user's role ("admin", "responder" or "viewer"), from currentRole.
	Role string
	// CanRespond is true for responder and admin: gates ack/unack UI.
	CanRespond bool
	// Name/Initial are the signed-in user's display name (User.Name) and its uppercased first
	// letter, rendered in the sidebar footer's identity block (#80).
	Name, Initial string
	// Active is the request path, used to mark the matching nav link class="active".
	Active string
	// Nav is Role's filtered nav list, precomputed so the template doesn't
	// need role-aware logic beyond the active-link comparison.
	Nav []NavItem
	// Nonce is this request's per-response CSP nonce (see security.go's
	// securityHeaders/nonceFromContext).
	Nonce string
	// CSRF is the current session's anti-CSRF token (requireCSRF, middleware.go).
	CSRF string
	// CoreVersion is the running core daemon's version (fetched over the control socket),
	// WebVersion is this web plugin's own compiled-in version.
	CoreVersion     string
	WebVersion      string
	VersionMismatch bool
	// Node is this request's fleet node scope (node_scope.go's nodeFrom(r)): the zero value's
	// ID=="self"/Self==true/Prefix=="" replica.
	Node nodeScope
	// FleetRole is this daemon's fleet role for the CURRENT request: "solo"/"master"/"child"
	// (config.RoleSolo/RoleMaster/RoleChild).
	FleetRole string
	// Banner is the replica banner for a page scoped to a genuinely remote fleet node -- nil
	// for every self-scoped page.
	Banner *NodeBanner
	// Link is a child daemon's link status to its master (core.LinkView, from
	// Fleet().Status().Link).
	Link *core.LinkView
	// MasterURL is a child daemon's configured master address (core.FleetStatus.MasterURL).
	MasterURL string
	// Switcher is the topbar node switcher/Ctrl-K palette's node list: up to switcherNodeCap
	// entries (self first, then down nodes, then the rest by name).
	Switcher []SwitcherNode
	// NodeLabel is the switcher button's own text: "this server" for a self-scoped page (solo,
	// a master's own view, a child's own view -- Node.Self).
	NodeLabel string
}

// switcherNodeCap is the most nodes the topbar switcher/Ctrl-K palette lists.
const switcherNodeCap = 20

// SwitcherNode is one entry in the topbar node switcher dropdown and its Ctrl-K palette
// counterpart: a fleet roster node projected for THIS request's own page.
type SwitcherNode struct {
	// ID is the roster id (core.NodeSummary.ID; core.SelfNodeID for self).
	ID string
	// Name is the display name: "this server" for self, NodeSummary.Name otherwise.
	Name string
	// State is NodeSummary.State, defaulting to "online" for self.
	State string
	// Self mirrors NodeSummary.Self.
	Self bool
	// Current reports whether this entry IS the page's own current node
	// scope (nodeScope.ID) -- the switcher/palette's "you are here" marker.
	Current bool
	// Href is this entry's link: the current page's type
	// (switcherTargetPath) under this node's own URL prefix (nodeHref).
	Href string
}

// buildSwitcherNodes projects nodes (the full, unfiltered fleet roster) into the
// switcher/palette's ordering and per-node Href: self first, then down nodes (by name).
func buildSwitcherNodes(nodes []core.NodeSummary, current nodeScope, targetPath string) []SwitcherNode {
	var self *core.NodeSummary
	var down, rest []core.NodeSummary
	for i := range nodes {
		n := nodes[i]
		switch {
		case n.Self:
			self = &n
		case n.State == "down":
			down = append(down, n)
		default:
			rest = append(rest, n)
		}
	}
	sort.Slice(down, func(i, j int) bool { return down[i].Name < down[j].Name })
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })

	ordered := make([]core.NodeSummary, 0, 1+len(down)+len(rest))
	if self != nil {
		ordered = append(ordered, *self)
	}
	ordered = append(ordered, down...)
	ordered = append(ordered, rest...)
	if len(ordered) > switcherNodeCap {
		ordered = ordered[:switcherNodeCap]
	}

	out := make([]SwitcherNode, 0, len(ordered))
	for _, n := range ordered {
		prefix, name := "", n.Name
		if n.Self {
			name = "this server"
		} else {
			prefix = "/n/" + n.ID
		}
		state := n.State
		if state == "" {
			state = "online"
		}
		out = append(out, SwitcherNode{
			ID:      n.ID,
			Name:    name,
			State:   state,
			Self:    n.Self,
			Current: n.ID == current.ID,
			Href:    nodeHref(prefix, targetPath),
		})
	}
	return out
}

// switcherTargetPath returns the page-type path (plus query string) every switcher/palette
// entry links to: p unchanged.
func switcherTargetPath(p, rawQuery string) string {
	p = path.Clean(p)
	if isMasterLocalPath(p) {
		return "/"
	}
	if rawQuery != "" {
		return p + "?" + rawQuery
	}
	return p
}

// nodeLabelFor is the switcher button text: "Go to node" on fleet-wide
// pages, "this server" on the host's own pages, else the node's name.
func nodeLabelFor(ns nodeScope, path string) string {
	if ns.Self && isMasterLocalPath(path) {
		return "Go to node"
	}
	if ns.Self {
		return "this server"
	}
	return ns.Name
}

// NodeBanner is the replica banner's render data: a compact summary of the remote node's
// own last-known state, rendered by base.html's "nodebanner" partial just under the topbar.
type NodeBanner struct {
	// Name is the node's display name (NodeSummary.Name).
	Name string
	// State is NodeSummary.State verbatim: "online", "lagging", "catching up", "stale",
	// "down", or "revoked" -- the banner's text and accent both switch on this.
	State string
	// LastSeen is NodeSummary.LastSeen (Unix seconds): the online banner's
	// "updated Xs ago" and the down/stale banner's "down since HH:MM".
	LastSeen int64
	// OutboxBytes is NodeSummary.OutboxBytes verbatim (bytes still queued
	// for this node), carried alongside the precomputed Behind text below.
	OutboxBytes int64
	// Behind is the catching-up/lagging banner's precomputed "N behind" clause, built from
	// NodeSummary.OutboxBytes/OutboxOldest by behindText -- empty when neither is known.
	Behind string
}

// newPageData builds the PageData every page handler needs, deriving Role from the request
// and Active from its path, and Status/StatusText from the real active-alert set.
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
	node := nodeFrom(r)
	fleetInfo := resolveFleetPageInfo(r, d)
	var switcher []SwitcherNode
	if fleetInfo.role == config.RoleMaster {
		// fleetMemoFrom(r).fleetNodes(d) is the SAME cached Fleet().Nodes() result every other
		// roster lookup this request makes already shares (fleet_memo.go).
		if nodes, err := fleetMemoFrom(r).fleetNodes(d); err == nil {
			switcher = buildSwitcherNodes(nodes, node, switcherTargetPath(r.URL.Path, r.URL.RawQuery))
		}
	}
	return PageData{
		Title:           title,
		Sub:             sub,
		ServerName:      d.Cfg().ServerName(),
		Status:          status,
		StatusText:      statusText,
		Role:            role,
		CanRespond:      roleRank(Role(role)) >= roleRank(RoleResponder),
		Name:            name,
		Initial:         firstInitial(name),
		Active:          nodeHref(node.Prefix, r.URL.Path),
		Nav:             navForRole(role, navCountsFor(r, d, fleetInfo.role), node, fleetInfo.role),
		Nonce:           nonceFromContext(r),
		CSRF:            csrf,
		CoreVersion:     coreVer,
		WebVersion:      webVer,
		VersionMismatch: coreVer != "" && coreVer != "unknown" && coreVer != webVer,
		Node:            node,
		FleetRole:       fleetInfo.role,
		Banner:          buildNodeBanner(node),
		Link:            fleetInfo.link,
		MasterURL:       fleetInfo.masterURL,
		Switcher:        switcher,
		NodeLabel:       nodeLabelFor(node, r.URL.Path),
	}
}

// fleetPageInfo is resolveFleetPageInfo's return shape: PageData's FleetRole/Link/MasterURL
// fields.
type fleetPageInfo struct {
	role      string
	link      *core.LinkView
	masterURL string
}

// resolveFleetPageInfo determines the current request's fleet role (and, for a child, its
// link status/master URL) for newPageData: call Fleet().Status() at most once per REQUEST.
func resolveFleetPageInfo(r *http.Request, d Deps) fleetPageInfo {
	if _, ok := r.Context().Value(nodeScopeCtxKey{}).(nodeScope); ok {
		return fleetPageInfo{role: config.RoleMaster}
	}
	status, err := fleetMemoFrom(r).fleetStatus(d)
	if err != nil || status.Role == "" {
		return fleetPageInfo{role: config.RoleSolo}
	}
	return fleetPageInfo{role: status.Role, link: status.Link, masterURL: status.MasterURL}
}

// buildNodeBanner returns the replica banner (NodeBanner) for a page scoped to a genuinely
// remote fleet node, nil for every self-scoped page.
func buildNodeBanner(ns nodeScope) *NodeBanner {
	if ns.Self {
		return nil
	}
	return &NodeBanner{
		Name:        ns.Name,
		State:       ns.Summary.State,
		LastSeen:    ns.Summary.LastSeen,
		OutboxBytes: ns.Summary.OutboxBytes,
		Behind:      behindText(ns.Summary),
	}
}

// behindText renders NodeBanner.Behind (the "catching up"/"lagging" banner's "N behind"
// clause) from NodeSummary.OutboxBytes/OutboxOldest: the queued byte count.
func behindText(n core.NodeSummary) string {
	var parts []string
	if n.OutboxBytes > 0 {
		parts = append(parts, humanBytes(uint64(n.OutboxBytes)))
	}
	if n.OutboxOldest > 0 {
		parts = append(parts, "oldest "+nodeAgoText(n.OutboxOldest))
	}
	return strings.Join(parts, ", ")
}

// coreVersionViaAPI fetches the running core daemon's version over the control socket
// (#107).
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

// renderPageStatus parses base.html together with the named page template.
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

// renderDenied renders the "Higher role needed" denied panel (templates/denied.html)
// through the full app-shell layout with a 403 status -- requireRole.
func renderDenied(w http.ResponseWriter, r *http.Request, d Deps) {
	data := newPageData(r, d, "Higher role needed", "Access denied")
	if err := renderPageStatus(w, "denied.html", data, http.StatusForbidden); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// renderNotFound is renderDenied's 404 counterpart (templates/notfound.html, same "panel
// denied" styling), through the full app-shell layout. reason is a short.
func renderNotFound(w http.ResponseWriter, r *http.Request, d Deps, reason string) {
	data := newPageData(r, d, "Not found", reason)
	if err := renderPageStatus(w, "notfound.html", data, http.StatusNotFound); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// BarePageData is what a "bare"/centered page (enroll, login, the public dashboard -- they
// use the centered `.card`/`.center` styling rather than the app shell) renders against.
type BarePageData struct {
	// Title feeds the <title> tag, same as PageData.Title.
	Title string
	// Nonce is this request's CSP nonce (see security.go), threaded onto
	// base_bare.html's boot script tag exactly like PageData.Nonce.
	Nonce string
	// EnrollToken is this request's ?token= query parameter, if any.
	EnrollToken string
	SignedIn    bool
	// EnrollClosed (U8, 2026-09-25 UI audit fix) is true when GET /enroll carries no ?token=
	// AND the user store already has at least one account: resolveEnrollRole.
	EnrollClosed bool
}

// newBarePageData builds the BarePageData a bare-layout page handler needs.
func newBarePageData(r *http.Request, title string) BarePageData {
	return BarePageData{
		Title:       title,
		Nonce:       nonceFromContext(r),
		EnrollToken: r.URL.Query().Get("token"),
		SignedIn:    signedIn(r),
	}
}

func signedIn(r *http.Request) bool {
	_, ok := userFromContext(r)
	return ok
}

// renderBarePage is renderPageStatus's counterpart for the bare/centered layout: it parses
// base_bare.html together with the named page template instead of base.html.
func renderBarePage(w http.ResponseWriter, page string, data BarePageData) error {
	tmpl, err := template.New("base_bare.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base_bare.html", "templates/"+page)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base_bare.html", data)
}
