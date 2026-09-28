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
	// nodeHref/nodeAgo/nodeDur/clockTime/linkUnreachable (task 3, node-aware
	// templates) back every same-origin link's node prefix and the replica
	// banner / child link badge's time and threshold formatting -- see each
	// func's own doc below.
	"nodeHref":        nodeHref,
	"nodeAgo":         nodeAgoText,
	"nodeDur":         nodeDurText,
	"clockTime":       nodeClockTime,
	"linkUnreachable": linkUnreachable,
	// dict (task C3, fleet_alerting.html) builds a map[string]any from
	// alternating key/value arguments, for passing a small ad-hoc bundle of
	// fields into a named template block ({{template "x" (dict "A" 1 "B"
	// 2)}}) -- html/template has no map literal syntax of its own.
	"dict": templateDict,
	// managedKeyKnown (task C5, fleet_managed.html) reports whether key is
	// one of core.ManagedKeys -- the create/edit form's key <select> uses it
	// to add a visible, selected fallback option for a row whose Key came
	// from a rejected raw POST naming something OUTSIDE the allowlist
	// (bypassing the select entirely, as any raw HTTP client could): without
	// this, that row's own <select> would silently show nothing selected on
	// re-render, losing the very input the error message is about
	// (global-constraints.md: "a validation error ... preserves ALL input").
	"managedKeyKnown": func(key string) bool {
		for _, k := range core.ManagedKeys {
			if k == key {
				return true
			}
		}
		return false
	},
}

// nodeHref joins a node scope's URL prefix (nodeScope.Prefix, node_scope.go:
// "" for self, "/n/<id>" for a remote node) with a same-origin route path,
// for every page-template link/form-action that must follow the current
// request's node scope (task 3, global-constraints.md's "remote nodes are
// read-only in the UI" plus the plan's "every same-origin link is node-
// prefixed" requirement). path must start with "/" -- every caller passes a
// literal route path, never a relative one, so this is plain concatenation:
// nodeHref("", "/monitoring") == "/monitoring" (self, byte-identical to
// before this task), nodeHref("/n/child1", "/monitoring") ==
// "/n/child1/monitoring".
func nodeHref(prefix, path string) string {
	return prefix + path
}

// nodeDurText renders a Unix timestamp as a short duration since now, with
// no "ago" suffix: "3s", "2m", "1h", "4d" (seconds precision below a
// minute, minute above -- mirroring the fleet CLI's own `ago` helper,
// internal/trinetra/fleet_cmd.go, which this package can't import across
// the internal/web -> internal/trinetra layering boundary). ts<=0 (never
// seen) renders "never". Used directly for the topbar child-link pill's
// "Master unreachable 12m" text, and as nodeAgoText's building block.
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

// nodeAgoText is nodeDurText with an " ago" suffix ("3s ago", "2m ago"),
// except ts<=0 which stays the bare "never" (an "never ago" reading would be
// wrong). Used for the replica banner's "updated Xs ago" and the child
// link pill's healthy "ack Xs ago".
func nodeAgoText(ts int64) string {
	d := nodeDurText(ts)
	if ts <= 0 {
		return d
	}
	return d + " ago"
}

// nodeClockTime renders a Unix timestamp as a local HH:MM clock reading, for
// the replica banner's "child1 is down since 14:02" text. ts<=0 renders
// "unknown" (no last-seen timestamp to show).
func nodeClockTime(ts int64) string {
	if ts <= 0 {
		return "unknown"
	}
	return time.Unix(ts, 0).Local().Format("15:04")
}

// linkUnreachableThreshold is the controller ruling's cutoff for the topbar
// child-link pill: a link retrying for longer than this renders the amber
// "Master unreachable" pill instead of the healthy verdigris "Linked to
// master" one.
const linkUnreachableThreshold = 2 * time.Minute

// linkUnreachable reports whether a child's link to its master (from
// Fleet().Status().Link, core.LinkView) has been retrying for longer than
// linkUnreachableThreshold. "retrying" is core.LinkView.State's literal
// value for that condition (internal/fleet.LinkRetrying's own value --
// internal/web must not import internal/fleet, so this compares the plain
// string core.LinkView already carries, the same convention core.NodeSummary
// .State comparisons use elsewhere in this package). A link with no
// LastAck at all (never once acked) counts as unreachable outright, since
// there is no better evidence it's healthy.
func linkUnreachable(l *core.LinkView) bool {
	if l == nil || l.State != "retrying" {
		return false
	}
	if l.LastAck <= 0 {
		return true
	}
	return time.Since(time.Unix(l.LastAck, 0)) > linkUnreachableThreshold
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

// navEntry is a NavItem plus the role/fleet-role gates navForRole filters
// on and never exposes to templates: AdminOnly mirrors the mockup NAV's
// `admin:true` flag; MasterOnly (task 5, fleet-web-a) is this daemon's own
// fleetRole gate for the "Fleet" entry -- solo/child daemons never show it
// at all (global-constraints.md: "no fleet nav" on solo/child).
type navEntry struct {
	NavItem
	AdminOnly bool
	// MasterOnly entries are ALSO exempt from node-prefixing (see
	// navForRole): unlike Dashboard/Monitoring/etc, "/fleet" is a
	// master-local URL (node_scope.go's masterLocalPrefixes) that's
	// reachable -- unprefixed -- from every page, including a remote
	// node's (task-3-brief.md's nodeLinkAllowlist already carried "/fleet"
	// in anticipation of this), not just the master's own self-scoped
	// pages the way AdminOnly entries are restricted to.
	MasterOnly bool
}

// navItems mirrors the mockup app.js NAV array verbatim (headings, paths,
// icons, labels, admin gating) with mockup .html paths swapped for the
// server's real routes. Badge values are deliberately NOT set here: the
// mockup's hardcoded demo counts (Monitoring 220 / Alerts 2 / Channels 5 /
// Users 3) are computed fresh per request instead (navCountsFor, badgeFor
// below) so they never go stale.
var navItems = []navEntry{
	{NavItem: NavItem{Heading: "Monitor"}},
	{NavItem: NavItem{Href: "/fleet", Icon: "⛶", Label: "Fleet"}, MasterOnly: true},
	// Incidents (task C2, fleet phase 2 web UI plan C): master only, visible
	// to viewers (no AdminOnly gate -- read-only for a viewer, ack/silence
	// are admin+CSRF-gated at the route/handler level), badged with the
	// firing count (badgeFor's "/fleet/incidents" case, nav_counts.go). Like
	// "/fleet" above, MasterOnly also exempts its Href from node-prefixing --
	// it only ever means "this master's own incidents".
	{NavItem: NavItem{Href: "/fleet/incidents", Icon: "⚠", Label: "Incidents"}, MasterOnly: true},
	// Silences (task C4, fleet phase 2 web UI plan C): master only, visible
	// to viewers -- the brief's own ruling ("'Silences' in the Monitor
	// group, master only, visible to viewers"). Read-only for a viewer
	// (every create/expire/delete mutation is admin+CSRF-gated at the
	// route/handler level, handlers_fleet_silences.go), exactly like
	// "Incidents" above.
	{NavItem: NavItem{Href: "/fleet/silences", Icon: "☾", Label: "Silences"}, MasterOnly: true},
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
	// Alerting (task C3, fleet phase 2 web UI plan C): the routing/
	// escalation config editor, route tester, and rule states. AdminOnly +
	// MasterOnly, exactly like "Fleet admin" below -- the brief's own
	// ruling ("Alerting in the Admin group, master only, visible to
	// admins"). A viewer gets no nav link (this entry) but the route
	// itself stays viewer-gated read-only (routes.go), reachable by typing
	// the URL directly -- deliberate: the brief left this an open call
	// ("decide whether the nav should show for viewers too, and document
	// it"), and every other Admin-group entry already hides from viewers
	// this same way, so a lone exception here would be the surprising
	// choice, not this one.
	{NavItem: NavItem{Href: "/fleet/alerting", Icon: "⚡", Label: "Alerting"}, AdminOnly: true, MasterOnly: true},
	// Fleet admin (Task 7, fleet-web-a): node management + join tokens.
	// Admin role AND fleet master, both required (global-constraints.md/
	// task-7-brief.md's ruling) -- AdminOnly gates on role (and, like every
	// other AdminOnly entry, on node.Self: it's a master-local page,
	// node_scope.go's masterLocalPrefixes), MasterOnly gates on fleetRole
	// exactly like the "/fleet" entry above, and (per MasterOnly's own doc)
	// also exempts this entry's Href from node-prefixing -- "/fleet/admin"
	// only ever means "this master's own admin page".
	{NavItem: NavItem{Href: "/fleet/admin", Icon: "⚑", Label: "Fleet admin"}, AdminOnly: true, MasterOnly: true},
	// Managed config (task C5, fleet phase 2 web UI plan C): the managed-
	// config fragment editor + per-node status. AdminOnly + MasterOnly,
	// exactly like "Fleet admin" above -- task-5-brief.md's ruling ("Managed
	// config in the Admin group, master only, admin only"). GET itself stays
	// viewer-gated read-only (routes.go), reachable by typing the URL
	// directly, matching "Alerting"'s precedent above.
	{NavItem: NavItem{Href: "/fleet/managed", Icon: "▥", Label: "Managed config"}, AdminOnly: true, MasterOnly: true},
	// Audit (task C5): the fleet audit log. AdminOnly + MasterOnly, per the
	// same ruling ("'Audit' in the Admin group, master only, admin only") --
	// unlike Managed config/Alerting, the audit log itself is admin-only
	// end to end (task-5-brief.md's route list has no viewer-readable GET
	// here), so there is no "hidden from viewers but still reachable by
	// URL" nuance to document.
	{NavItem: NavItem{Href: "/fleet/audit", Icon: "▦", Label: "Audit"}, AdminOnly: true, MasterOnly: true},
}

// navForRole returns navItems filtered to what role may see (viewers get
// everything except AdminOnly entries, admins get everything -- the
// server-side equivalent of the mockup app.js NAV.filter(role==='admin' ||
// !n.admin)) with each entry's Badge filled in from counts via badgeFor,
// and (task 3) every entry's Href carrying node's URL prefix.
//
// node additionally gates the AdminOnly entries (the "Admin" heading plus
// Configuration/Channels/Users/Public view): on a remote node's page
// (node.Self == false) they're hidden outright, regardless of role --
// config/channels/users/public-settings are master-local pages
// (masterLocalPrefixes, node_scope.go) that only ever mean "this master",
// so they stay reachable from the master's own (self-scoped) nav, never
// from a node-scoped one (global-constraints.md, task-3-brief.md).
//
// badgeFor is deliberately called with the entry's ORIGINAL, unprefixed
// Href (its switch matches literal paths like "/monitoring") -- prefixing
// happens after, so a remote node's Monitoring badge still resolves
// correctly instead of silently going blank because "/n/child1/monitoring"
// never matches badgeFor's cases.
//
// fleetRole (task 5, fleet-web-a) gates MasterOnly entries: "/fleet" shows
// only when this daemon is a fleet master (config.RoleMaster), regardless
// of node/role -- see navEntry.MasterOnly's doc for why its Href is also
// exempt from the node.Prefix join every other entry gets.
func navForRole(role string, counts NavCounts, node nodeScope, fleetRole string) []NavItem {
	out := make([]NavItem, 0, len(navItems))
	for _, n := range navItems {
		if n.AdminOnly && (role != "admin" || !node.Self) {
			continue
		}
		if n.MasterOnly && fleetRole != config.RoleMaster {
			continue
		}
		item := n.NavItem
		item.Badge = badgeFor(item.Href, counts)
		if item.Href != "" && !n.MasterOnly {
			item.Href = nodeHref(node.Prefix, item.Href)
		}
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
	case "/fleet":
		return badgeText(counts.FleetDown)
	case "/fleet/incidents":
		return badgeText(counts.IncidentsFiring)
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
	// Node is this request's fleet node scope (node_scope.go's nodeFrom(r)):
	// the zero value's ID=="self"/Self==true/Prefix=="" replica -- ordinary
	// solo/master-self/child pages all render identically to before task 3,
	// since nodeHref(node.Prefix, path) with an empty Prefix is a no-op.
	// Non-zero (Self==false, Prefix=="/n/<id>") only on a master's page for
	// a genuinely remote node.
	Node nodeScope
	// FleetRole is this daemon's fleet role for the CURRENT request:
	// "solo"/"master"/"child" (config.RoleSolo/RoleMaster/RoleChild). Drives
	// the topbar child-link pill (FleetRole=="child") -- see
	// resolveFleetPageInfo's doc for how this is derived without an extra
	// Fleet().Status() round trip on a page already known to be a master's
	// (a node-scoped request, Node.Self==false).
	FleetRole string
	// Banner is the replica banner (task 3) for a page scoped to a genuinely
	// remote fleet node -- nil for every self-scoped page (solo, a master's
	// own view, a child's own view, or a node-scoped page redirected back to
	// self). See buildNodeBanner's doc.
	Banner *NodeBanner
	// Link is a child daemon's link status to its master (core.LinkView,
	// from Fleet().Status().Link), non-nil only when FleetRole=="child" and
	// that Status() call actually reported one. Drives the topbar's "Linked
	// to master"/"Master unreachable" pill.
	Link *core.LinkView
	// MasterURL is a child daemon's configured master address
	// (core.FleetStatus.MasterURL), surfaced as the child-link pill's
	// tooltip so an operator can see exactly where "master" points without
	// following an external link out of this page.
	MasterURL string
	// Switcher is the topbar node switcher/Ctrl-K palette's node list (task
	// 6, fleet-web-a): up to switcherNodeCap entries (self first, then down
	// nodes, then the rest by name), each carrying this SPECIFIC page's
	// link under that node's prefix -- see buildSwitcherNodes' doc. Nil
	// (base.html renders neither the switcher button nor the #nodePalette
	// markup at all) unless FleetRole==config.RoleMaster: per the
	// controller ruling, solo and child daemons get no switcher and no
	// palette, full stop.
	Switcher []SwitcherNode
	// NodeLabel is the switcher button's own text: "this server" for a
	// self-scoped page (solo, a master's own view, a child's own view --
	// Node.Self), the node's display name otherwise. See nodeLabelFor.
	NodeLabel string
}

// switcherNodeCap is the topbar switcher/Ctrl-K palette's ruling: list up
// to 20 nodes.
const switcherNodeCap = 20

// SwitcherNode is one entry in the topbar node switcher dropdown and its
// Ctrl-K palette counterpart (task 6, fleet-web-a): a fleet roster node
// projected for THIS request's own page -- its Href already carries the
// current page's "type" (e.g. /monitoring) under that node's own prefix,
// per switcherTargetPath's doc, so a template/click handler never needs to
// re-derive it.
type SwitcherNode struct {
	// ID is the roster id (core.NodeSummary.ID; core.SelfNodeID for self).
	ID string
	// Name is the display name: "this server" for self, NodeSummary.Name
	// otherwise (never empty -- a remote node with no configured name still
	// carries its NodeSummary.Name, which fleet enrollment always sets).
	Name string
	// State is NodeSummary.State, defaulting to "online" for self (whose
	// roster entry may leave State unset -- self's own health is already
	// reported by the topbar's own status pill, not this list). Always
	// rendered as plain text next to the led dot -- never color-only.
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

// buildSwitcherNodes projects nodes (the full, unfiltered fleet roster) into
// the switcher/palette's ordering and per-node Href, per the controller
// ruling: self first, then down nodes (by name), then the rest (by name),
// capped at switcherNodeCap. current is this request's own node scope (for
// the Current flag); targetPath is the page-type path every entry links to
// under its own node's prefix (switcherTargetPath's result -- already
// swapped to "/" for a master-local current page).
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

// switcherTargetPath returns the page-type path (plus query string) every
// switcher/palette entry links to: p unchanged (already node-prefix-stripped
// -- see nodeFrom's doc, and newPageData's caller which passes r.URL.Path
// directly) with "?"+rawQuery appended when set (round-1 review: switching
// nodes from e.g. /history?metric=cpu must keep ?metric=cpu, not silently
// drop it) for an ordinary node-scoped page; or "/" alone, with NO query
// string, when p falls under node_scope.go's masterLocalPrefixes --
// config/channels/users/settings/fleet/etc. have no per-node counterpart to
// switch to at all (task 6's ruling: "master-local pages... switch to the
// node's dashboard instead"), and a filter/sort query tied to a
// master-local page (e.g. /fleet?state=down) has no meaning on a node's own
// dashboard.
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

// nodeLabelFor renders PageData.NodeLabel for ns: "this server" for a
// self-scoped page (solo, a master's own view, a child's own view), ns.Name
// otherwise.
func nodeLabelFor(ns nodeScope) string {
	if ns.Self {
		return "this server"
	}
	return ns.Name
}

// NodeBanner is the replica banner's render data (task 3, task-3-brief.md's
// exact interface): a compact summary of the remote node's own last-known
// state, rendered by base.html's "nodebanner" partial just under the
// topbar. Built by buildNodeBanner from the current request's nodeScope
// (node_scope.go), which already carries the roster's NodeSummary for the
// scoped node (fetched once by withNodeRouter/resolveMasterAndNodes to
// validate the {node} path segment -- no extra Fleet() round trip here).
type NodeBanner struct {
	// Name is the node's display name (NodeSummary.Name).
	Name string
	// State is NodeSummary.State verbatim: "online", "lagging", "catching
	// up", "stale", "down", or "revoked" -- the banner's text and accent
	// both switch on this.
	State string
	// LastSeen is NodeSummary.LastSeen (Unix seconds): the online banner's
	// "updated Xs ago" and the down/stale banner's "down since HH:MM".
	LastSeen int64
	// OutboxBytes is NodeSummary.OutboxBytes verbatim (bytes still queued
	// for this node), carried alongside the precomputed Behind text below.
	OutboxBytes int64
	// Behind is the catching-up/lagging banner's precomputed "N behind"
	// clause, built from NodeSummary.OutboxBytes/OutboxOldest by
	// behindText -- empty when neither is known, in which case the banner
	// omits the clause entirely rather than rendering a bare ", behind".
	Behind string
}

// newPageData builds the PageData every page handler needs, deriving Role
// from the request and Active from its path, and Status/StatusText from the
// real active-alert set (topbarStatus(activeAlertsViaAPI(d))) --
// see PageData's doc for why every page shares this one computation rather
// than each supplying its own. d is also used to compute the nav's live
// badge counts (navCountsFor); every other field is unchanged from the
// request/session.
//
// Active carries the current node scope's prefix (task 3): a bare
// r.URL.Path would no longer match a remote node's now-prefixed Nav hrefs
// (navForRole), breaking the sidebar/mobile-nav "active" highlight on every
// node-scoped page -- reconstructing the full node-scoped path here keeps
// the comparison correct, and is a no-op (Prefix=="") for every self-scoped
// page exactly as before this task.
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
		// fleetMemoFrom(r).fleetNodes(d) is the SAME cached Fleet().Nodes()
		// result every other roster lookup this request makes already
		// shares (fleet_memo.go) -- building the switcher here costs a real
		// round trip only when nothing else in the request already paid for
		// one.
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
		NodeLabel:       nodeLabelFor(node),
	}
}

// fleetPageInfo is resolveFleetPageInfo's return shape: PageData's
// FleetRole/Link/MasterURL fields, computed together so the (at most one)
// Fleet().Status() call this request makes for page rendering serves all
// three.
type fleetPageInfo struct {
	role      string
	link      *core.LinkView
	masterURL string
}

// resolveFleetPageInfo determines the current request's fleet role (and, for
// a child, its link status/master URL) for newPageData, honoring the
// controller ruling: call Fleet().Status() at most once per REQUEST (not
// per call site -- see fleet_memo.go), and only when the role isn't already
// known from resolveMasterAndNodes.
//
// A request that reached here through withNodeRouter (node_scope.go) --
// i.e. nodeScopeCtxKey{} is set in its context -- already proved this
// daemon a master (withNodeRouter only proceeds past resolveMasterAndNodes
// when isMaster is true), so that case returns "master" outright with no
// further round trip -- not even a memoized one, since resolveMasterAndNodes
// may well have settled "master" from Nodes() alone without ever calling
// Status() at all. Every other request (the vast majority: every
// unprefixed page, since withNodeRouter only inspects /n/... paths at all)
// falls through to r's fleetMemo, collapsing every failure mode (nil
// Deps.Fleet, a nil FleetAPI, a Status() error, or an empty Role -- an old
// daemon predating Fleet.Status) to "solo", exactly like fleetRole's doc
// explains for the same failure set.
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

// buildNodeBanner returns the replica banner (NodeBanner) for a page scoped
// to a genuinely remote fleet node, nil for every self-scoped page (see
// PageData.Node's doc). ns.Summary is the roster's NodeSummary for the
// scoped node, already resolved by withNodeRouter/resolveMasterAndNodes to
// validate the {node} path segment -- building the banner from it costs no
// extra Fleet() round trip.
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

// behindText renders NodeBanner.Behind (the "catching up"/"lagging" banner's
// "N behind" clause) from NodeSummary.OutboxBytes/OutboxOldest (the
// controller ruling's exact source fields): the queued byte count
// (humanBytes) and, when known, how long the oldest queued record has been
// waiting ("oldest Xm ago", via nodeAgoText). Empty when neither is known,
// so the banner can omit the clause entirely rather than render a bare
// trailing ", behind".
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
