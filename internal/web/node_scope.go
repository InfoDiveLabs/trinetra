package web

import (
	"context"
	"net/http"
	"path"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// nodeScopeCtxKey is the unexported context key withNodeRouter stores the
// resolved nodeScope under for a request dispatched through /n/{node}/...
// A request that never went through that prefix has no value under this
// key at all -- nodeFrom reports the implicit self scope for it instead.
type nodeScopeCtxKey struct{}

// nodeScope is what a page handler sees once nodeFrom(r) resolves the
// current request: which fleet node it's scoped to, and enough to render a
// node-aware page (a link prefix, a display name) without every handler
// needing to reach back into the fleet roster itself.
type nodeScope struct {
	// ID is the registry id this request is scoped to: core.SelfNodeID
	// ("self") for the master's own node -- true both for a plain
	// unprefixed request (the implicit scope) and, briefly, for a
	// /n/self/... request, which withNodeRouter 308-redirects to the bare
	// path before any handler ever observes this scope.
	ID string
	// Name is the node's display name (NodeSummary.Name); "" for the
	// implicit self scope, where there is no fleet roster lookup to name it
	// from.
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

// nodeFrom returns the current request's node scope: whatever
// withNodeRouter attached to the context, or the implicit self scope for a
// request on its own unprefixed path (every route that existed before fleet
// routing, unchanged).
func nodeFrom(r *http.Request) nodeScope {
	if ns, ok := r.Context().Value(nodeScopeCtxKey{}).(nodeScope); ok {
		return ns
	}
	return nodeScope{ID: core.SelfNodeID, Self: true}
}

// apiFor returns the core.API a page handler should read/write through for
// this request: d.API for the self scope (identical to every handler's
// pre-fleet behavior), or d.NodeAPI(id) once withNodeRouter has scoped the
// request to a remote node. Falls back to d.API when d.NodeAPI is nil (no
// fleet routing wired) even for a non-self scope, so a caller that gets a
// nil result here treats it exactly like Deps.API's own documented "may be
// nil, callers must check" contract rather than a new failure mode.
func apiFor(r *http.Request, d Deps) core.API {
	ns := nodeFrom(r)
	if ns.Self || d.NodeAPI == nil {
		return d.API
	}
	return d.NodeAPI(ns.ID)
}

// fleetRole reports this daemon's fleet role as config.RoleSolo/RoleMaster/
// RoleChild, via r's request-scoped fleetMemo (fleet_memo.go) -- a single
// Fleet().Status() call per REQUEST, not per call site: every failure mode
// collapses to RoleSolo -- d.Fleet unset, a nil FleetAPI, Status() erroring
// (an old daemon predating Fleet.Status entirely), or an empty Role. This is
// the general-purpose "what's my role" answer (e.g. handlers_fleet.go's
// masters-only gate); withNodeRouter itself uses the more roster-aware
// resolveMasterAndNodes below, since it needs the node roster too -- both
// share the same memo, so a request that calls into both still costs at
// most one Status() and one Nodes() call total.
func fleetRole(r *http.Request, d Deps) string {
	status, err := fleetMemoFrom(r).fleetStatus(d)
	if err != nil || status.Role == "" {
		return config.RoleSolo
	}
	return status.Role
}

// resolveMasterAndNodes determines, in as few Fleet() round trips as
// practical, whether this daemon is a fleet master and (when it is) its
// node roster -- exactly what withNodeRouter needs per request, and the
// only two questions it needs answered before deciding whether to
// intercept a /n/... request at all. Both the Nodes() and (when reached)
// Status() calls below go through r's request-scoped fleetMemo
// (fleet_memo.go), so this costs a real round trip only the FIRST time
// either is asked for anywhere in this request -- a later call site in the
// same request (fleetRole, resolveFleetPageInfo, navCountsFor,
// handlers_fleet.go's handlers) reuses the cached result instead of making
// its own.
//
// Nodes() alone already proves "master" the moment it reports any node
// besides self: a solo daemon's (or a plain, pre-fleet daemon's) roster is
// always just [self] (see fleet_provider.go's Nodes implementation), so
// finding a second entry needs no further confirmation. Status() -- the
// only way to tell solo apart from a genuine master with zero children so
// far -- is called only when Nodes() didn't already settle it (it returned
// only self, or errored), keeping the common "master with at least one
// node" case down to a single Fleet() round trip that also directly serves
// the node lookup withNodeRouter needs next.
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

// findNode returns the entry in nodes (already fetched by
// resolveMasterAndNodes -- never re-fetched here) whose ID exactly matches
// id.
func findNode(nodes []core.NodeSummary, id string) (core.NodeSummary, bool) {
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return core.NodeSummary{}, false
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

// masterLocalPrefixes are the path prefixes that never get node-scoped, even
// under /n/{node}/...: the admin config/channel/user editors, auth
// ceremonies, static assets, the public-status surface, and the (later
// task's) /fleet overview -- pages that only ever mean "this master", not
// "this master's view of node X". A request for one of these under a node
// prefix 404s rather than silently rendering the master's own page under
// what looks like a per-node URL (global-constraints.md).
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

// withNodeRouter wraps mux (the full route table newHandler builds) with
// /n/{node}/... support. A request whose path starts with /n/ has its node
// id resolved against the live fleet roster, the id stripped off, and the
// resulting nodeScope attached to the request context before re-dispatching
// into mux -- so every handler mux already routes to (requireRole gate and
// all) runs completely unchanged, just against a request whose context now
// names a different node and whose data-reading code can ask apiFor for the
// right core.API. A request whose path doesn't start with /n/ passes
// straight through to mux untouched.
//
// It is wired into newHandler just inside userMiddleware (routes.go), so
// requireRole/requireCSRF on the re-dispatched request still see the same
// session/user the outer middleware already resolved, and this handler
// itself can read userFromContext(r) the same way requireRole does.
//
// Node routing is deliberately narrow (global-constraints.md: remote nodes
// are read-only in the UI), and -- following round-1 review -- deliberately
// cheap/side-effect-free for anyone who shouldn't see it at all:
//
//   - Non-master daemons (solo, child, or Deps without Fleet wired at all)
//     never get intercepted, full stop: the request is hand off to mux
//     completely untouched, with no rendering and no core.API call of any
//     kind -- exactly the pre-fleet stdlib 404 an unmatched /n/... path
//     already got, for any HTTP method, signed in or not. This is checked
//     before anything else, via resolveMasterAndNodes.
//   - On a master, an anonymous caller (no session/user resolved by the
//     outer middleware -- userFromContext(r)) is redirected 302 to /login
//     for ANY /n/... path, before any node id is even parsed out of the
//     path, let alone looked up or rendered -- the same "no session at all"
//     outcome requireRole gives every other viewer+ route. Only a
//     signed-in request ever reaches a node lookup or the styled
//     not-found page (renderNotFound, which -- like every other page --
//     calls newPageData, which calls into core.API).
//   - A bare /n/{id} (no trailing slash) 308-redirects to /n/{id}/.
//   - Only GET/HEAD are node-routable at all -- every other method 404s
//     under a node prefix. GET /events IS node-routable as of Task 4
//     (fleet-web-a): eventsHandler (sse.go) switches to a poll-only loop
//     over apiFor(r,d).Snapshot() once it sees a non-self nodeFrom(r),
//     rather than calling Deps.Subscribe (there is still no per-node live
//     push over the control socket -- Subscribe stays scoped to this
//     daemon's own event bus).
//   - The sub-path is rejected outright (404) if it contains a literal ".."
//     segment (checked before any cleaning -- see containsDotDotSegment),
//     then path.Clean'd, before the masterLocalPrefixes check runs against
//     it: a normalized path is what that check (and the final dispatch)
//     sees, but a path that tried to smuggle ".." through it is rejected
//     rather than silently resolved.
//   - masterLocalPrefixes never get node-scoped (see its own doc).
//   - id == core.SelfNodeID ("self") redirects (308, preserving the query
//     string) to the bare unprefixed path: /n/self/x is never a distinct
//     page from /x, just an alternate spelling a future node switcher can
//     link to uniformly.
//   - a non-self id not found in the fleet roster (resolveMasterAndNodes'
//     nodes, from the SAME Fleet().Nodes() call already used to confirm
//     master status where possible -- see its own doc) 404s with reason
//     "no such node" -- ForNode/Node never fail locally for an unknown id
//     (see control.Client.ForNode's doc), so this exact-ID-match lookup
//     against the live roster is what actually validates the id before any
//     handler runs.
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

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			renderNotFound(w, r, d, "not found")
			return
		}

		subPath := "/" + sub
		if containsDotDotSegment(subPath) {
			renderNotFound(w, r, d, "not found")
			return
		}
		subPath = path.Clean(subPath)

		// /events is node-routable as of Task 4 (see this function's doc):
		// no exclusion here any more, it falls through to the ordinary
		// masterLocalPrefixes/self-redirect/roster-lookup path below like
		// every other GET.
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
