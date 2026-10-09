package web

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// nodeScopeCtxKey is the unexported context key withNodeRouter stores the resolved
// nodeScope under for a request dispatched through /n/{node}/...
type nodeScopeCtxKey struct{}

// nodeScope is what a page handler sees once nodeFrom(r) resolves the
// current request: which fleet node it's scoped to, and enough to render a
// node-aware page (a link prefix, a display name) without every handler
// needing to reach back into the fleet roster itself.
type nodeScope struct {
	// ID is the registry id this request is scoped to: core.SelfNodeID ("self") for the
	// master's own node -- true both for a plain unprefixed request.
	ID string
	// Name is the node's display name (NodeSummary.Name); "" for the implicit self scope,
	// where there is no fleet roster lookup to name it from.
	Name string
	// Self reports whether ID == core.SelfNodeID.
	Self bool
	// Prefix is the URL prefix internal links should be rendered under:
	// "" for self, "/n/<id>" for a remote node.
	Prefix string
	// Summary is the fleet roster's full projection of this node
	// (NodeSummary) -- zero-valued for the implicit self scope.
	Summary core.NodeSummary
}

// nodeFrom returns the current request's node scope: whatever withNodeRouter attached to
// the context, or the implicit self scope for a request on its own unprefixed path.
func nodeFrom(r *http.Request) nodeScope {
	if ns, ok := r.Context().Value(nodeScopeCtxKey{}).(nodeScope); ok {
		return ns
	}
	return nodeScope{ID: core.SelfNodeID, Self: true}
}

// apiFor returns the core.API a page handler should read/write through for this request:
// d.API for the self scope (identical to every handler's pre-fleet behavior).
func apiFor(r *http.Request, d Deps) core.API {
	ns := nodeFrom(r)
	if ns.Self || d.NodeAPI == nil {
		return d.API
	}
	return d.NodeAPI(ns.ID)
}

// fleetRole reports this daemon's fleet role as config.RoleSolo/RoleMaster/ RoleChild, via
// r's request-scoped fleetMemo (fleet_memo.go).
func fleetRole(r *http.Request, d Deps) string {
	status, err := fleetMemoFrom(r).fleetStatus(d)
	if err != nil || status.Role == "" {
		return config.RoleSolo
	}
	return status.Role
}

// resolveMasterAndNodes determines, in as few Fleet() round trips as practical, whether
// this daemon is a fleet master and (when it is) its node roster.
func resolveMasterAndNodes(r *http.Request, d Deps) (isMaster bool, nodes []core.NodeSummary) {
	memo := fleetMemoFrom(r)
	if ns, err := memo.fleetNodes(d); err == nil {
		nodes = ns
		for _, n := range ns {
			if n.ID != core.SelfNodeID {
				return true, nodes
			}
		}
	}
	status, err := memo.fleetStatus(d)
	if err != nil || status.Role != config.RoleMaster {
		return false, nodes
	}
	return true, nodes
}

// findNode returns the entry in nodes (already fetched by resolveMasterAndNodes -- never
// re-fetched here) whose ID exactly matches id.
func findNode(nodes []core.NodeSummary, id string) (core.NodeSummary, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return core.NodeSummary{}, false
}

// nodeScopedAlertAckPath reports whether p (already stripped of its
// /n/{node} prefix, "/"-prefixed, NOT YET path.Clean'd) is EXACTLY
// "/alerts/{key}/ack" or "/alerts/{key}/unack" for some non-empty {key} --
// the one deliberate exception to "node-scoped routes are GET/HEAD only":
// remote alert ack/unack re-dispatches as a POST through to
// POST /alerts/{key}/ack|unack (routes.go), same as every other
// node-scoped GET re-dispatches to its own top-level route, so the handler
// resolves apiFor(r,d) to the right node. Every OTHER node-scoped path stays
// GET/HEAD only. Checked against the RAW (uncleaned) sub path, like the
// ".."-segment check in withNodeRouter, before path.Clean could normalize away
// something that looked like this shape.
func nodeScopedAlertAckPath(p string) bool {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	return len(parts) == 3 && parts[0] == "alerts" && parts[1] != "" && (parts[2] == "ack" || parts[2] == "unack")
}

// containsDotDotSegment reports whether p, split on "/", has a literal ".."
// path segment. r.URL.Path is always the percent-decoded form (verified:
// both a literal "/a/../b" and an escaped "/a/%2e%2e/b" request target
// arrive here as Path == "/a/../b"), so checking Path's segments catches a
// raw ".." and a percent-encoded one identically -- there is no separate
// "raw" form that could hide one from this check.
func containsDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// masterLocalPrefixes are the path prefixes that never get node-scoped, even under
// /n/{node}/...: the admin config/channel/user editors, auth ceremonies, static assets.
var masterLocalPrefixes = []string{
	"/config",
	"/channels",
	"/users",
	"/settings",
	"/enroll",
	"/login",
	"/logout",
	"/assets",
	"/public",
	"/fleet",
	"/status-page",
	"/status",
}

// isMasterLocalPath reports whether p (already stripped of its /n/{node}
// prefix and path.Clean'd) falls under one of masterLocalPrefixes.
func isMasterLocalPath(p string) bool {
	for _, prefix := range masterLocalPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// withNodeRouter wraps mux (the full route table newHandler builds) with /n/{node}/...
// support.
func withNodeRouter(d Deps, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/n/") {
			mux.ServeHTTP(w, r)
			return
		}

		isMaster, nodes := resolveMasterAndNodes(r, d)
		if !isMaster {
			mux.ServeHTTP(w, r)
			return
		}

		if _, ok := userFromContext(r); !ok {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, "/n/")
		id, sub, ok := strings.Cut(rest, "/")
		if id == "" {
			// "/n/" alone, or similarly degenerate -- doesn't match the
			// documented /n/{node}/... shape at all.
			mux.ServeHTTP(w, r)
			return
		}
		if !ok {
			// Bare /n/{id}, no trailing slash: normalize it.
			target := "/n/" + id + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
			return
		}

		rawSubPath := "/" + sub
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// The one deliberate exception: a POST to exactly /alerts/{key}/ack or
			// /alerts/{key}/unack re-dispatches through, same as any other node-scoped path.
			if !(r.Method == http.MethodPost && nodeScopedAlertAckPath(rawSubPath)) {
				renderNotFound(w, r, d, "not found")
				return
			}
		}

		subPath := rawSubPath
		if containsDotDotSegment(subPath) {
			renderNotFound(w, r, d, "not found")
			return
		}
		subPath = path.Clean(subPath)

		// /events is node-routable (see this function's doc): it falls through to the ordinary
		// masterLocalPrefixes/self-redirect/roster-lookup path below like every other GET.
		if isMasterLocalPath(subPath) {
			renderNotFound(w, r, d, "not found")
			return
		}

		if id == core.SelfNodeID {
			target := subPath
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
			return
		}

		summary, found := findNode(nodes, id)
		if !found {
			renderNotFound(w, r, d, "no such node")
			return
		}

		scope := nodeScope{
			ID:      id,
			Name:    summary.Name,
			Self:    false,
			Prefix:  "/n/" + id,
			Summary: summary,
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = subPath
		r2.URL.RawPath = ""
		r2 = r2.WithContext(context.WithValue(r2.Context(), nodeScopeCtxKey{}, scope))
		mux.ServeHTTP(w, r2)
	})
}
