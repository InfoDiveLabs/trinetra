package web

import (
	"context"
	"net/http"
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
// RoleChild. Every failure mode collapses to RoleSolo -- d.Fleet unset, a
// nil FleetAPI, Status() erroring (an old daemon predating Fleet.Status
// entirely), or an empty Role -- so a node router built on this treats all
// of them exactly like a genuinely solo daemon: no /n/{node}/... routing.
// Called fresh per request; this task deliberately adds no caching (see
// task brief).
func fleetRole(d Deps) string {
	if d.Fleet == nil {
		return config.RoleSolo
	}
	fleet := d.Fleet()
	if fleet == nil {
		return config.RoleSolo
	}
	status, err := fleet.Status()
	if err != nil || status.Role == "" {
		return config.RoleSolo
	}
	return status.Role
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
// prefix) falls under one of masterLocalPrefixes.
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
// id resolved against the live fleet roster (d.Fleet().Nodes), the id
// stripped off, and the resulting nodeScope attached to the request context
// before re-dispatching into mux -- so every handler mux already routes to
// (requireRole gate and all) runs completely unchanged, just against a
// request whose context now names a different node and whose data-reading
// code can ask apiFor for the right core.API. A request whose path doesn't
// start with /n/ passes straight through to mux untouched.
//
// It is wired into newHandler just inside userMiddleware (routes.go), so
// requireRole/requireCSRF on the re-dispatched request still see the same
// session/user the outer middleware already resolved -- node scoping never
// bypasses auth.
//
// Node routing is deliberately narrow (global-constraints.md: remote nodes
// are read-only in the UI):
//   - only GET/HEAD are node-routable at all, and GET /events is excluded
//     even though it's GET (a routed/non-self Subscribe is already refused
//     server-side, and there is no per-node live push yet) -- every other
//     method, plus /events, 404s under a node prefix rather than acting on
//     the master or failing deep inside a handler that assumed self.
//   - masterLocalPrefixes never get node-scoped (see its own doc).
//   - id == core.SelfNodeID ("self") redirects (308, preserving the query
//     string) to the bare unprefixed path: /n/self/x is never a distinct
//     page from /x, just an alternate spelling a future node switcher can
//     link to uniformly. This is checked (and honored) regardless of fleet
//     role -- self always resolves, even on a solo daemon.
//   - on a solo daemon (fleetRole == config.RoleSolo, including every
//     fleetRole failure mode) every non-self /n/... path 404s: there is no
//     fleet roster to resolve an id against.
//   - a non-self id not found in d.Fleet().Nodes(NodeFilter{}) (including
//     that call itself erroring) 404s with reason "no such node" --
//     ForNode/Node never fail locally for an unknown id (see
//     control.Client.ForNode's doc), so this exact-ID-match lookup against
//     the live roster is what actually validates the id before any handler
//     runs.
func withNodeRouter(d Deps, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/n/") {
			mux.ServeHTTP(w, r)
			return
		}

		rest := strings.TrimPrefix(r.URL.Path, "/n/")
		id, sub, ok := strings.Cut(rest, "/")
		if !ok || id == "" {
			// Doesn't actually match the documented /n/{node}/... shape
			// (e.g. a bare "/n/child1" with no trailing segment) -- fall
			// through to the plain mux, which 404s it the ordinary way.
			mux.ServeHTTP(w, r)
			return
		}
		subPath := "/" + sub

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			renderNotFound(w, r, d, "not found")
			return
		}
		if subPath == "/events" {
			renderNotFound(w, r, d, "not found")
			return
		}
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

		if fleetRole(d) == config.RoleSolo {
			renderNotFound(w, r, d, "no such node")
			return
		}

		summary, found := lookupNode(d, id)
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

// lookupNode resolves id against d.Fleet().Nodes(NodeFilter{}) by exact ID
// match, reporting (zero value, false) for any failure along the way (no
// Fleet wired, a nil FleetAPI, Nodes() erroring, or no matching ID) -- every
// one of those means "can't confirm this id exists", which withNodeRouter
// treats identically: 404.
func lookupNode(d Deps, id string) (core.NodeSummary, bool) {
	if d.Fleet == nil {
		return core.NodeSummary{}, false
	}
	fleet := d.Fleet()
	if fleet == nil {
		return core.NodeSummary{}, false
	}
	nodes, err := fleet.Nodes(core.NodeFilter{})
	if err != nil {
		return core.NodeSummary{}, false
	}
	for _, n := range nodes {
		if n.ID == id {
			return n, true
		}
	}
	return core.NodeSummary{}, false
}
