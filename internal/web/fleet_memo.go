package web

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// errNoFleetSupport is fleetMemo's internal stand-in for "this Deps has no
// fleet wiring at all" (a nil Deps.Fleet, or a nil core.FleetAPI once
// called) -- every caller of status/nodes below already treats a non-nil
// error identically to "solo"/"empty roster" (see fleetRole/
// resolveMasterAndNodes/resolveFleetPageInfo's own docs), so this never
// needs to be distinguished from a genuine backend error.
var errNoFleetSupport = errors.New("web: no fleet support")

// errNoAPI is fleetMemo's internal stand-in for "apiFor(r,d) returned nil"
// (no core.API wired for this request's node scope at all) -- snapshot's
// own caller (buildDashboardPageData) distinguishes this from a genuine
// Snapshot() error (it doesn't log for the former, the same tolerance the
// pre-memo inline `if api := apiFor(r, d); api != nil` check already had).
var errNoAPI = errors.New("web: no api support")

// fleetMemoCtxKey is the unexported context key withFleetMemo stores this
// request's *fleetMemo under.
type fleetMemoCtxKey struct{}

// fleetMemo caches THIS request's Fleet().Status() and Fleet().Nodes(core.
// NodeFilter{}) results, each computed at most once no matter how many
// different call sites ask for it (withNodeRouter's resolveMasterAndNodes,
// fleetRole, templates.go's resolveFleetPageInfo, nav_counts.go's
// navCountsFor, and every handlers_fleet.go handler all used to make their
// own independent Fleet() round trip -- round-1 review of Task 5 found GET
// /fleet alone making 2x Status() + 2x Nodes(); this is the fix).
//
// sync.Once (rather than a plain bool+mutex) both makes "compute once,
// cache forever for the life of this value" the obviously-correct behavior
// and is safe even if a future caller ever invoked it from more than one
// goroutine for the same request, which nothing here does today (request
// handling in this package is single-goroutine per request).
type fleetMemo struct {
	statusOnce sync.Once
	status     core.FleetStatus
	statusErr  error

	nodesOnce sync.Once
	nodes     []core.NodeSummary
	nodesErr  error

	// incidentsFiringOnce/incidentsFiringCount/incidentsFiringErr (task C2,
	// fleet incidents) memoize fleetIncidentsFiringCount's own
	// Incidents(State:"firing", Limit:fleetIncidentsFiringCap) call -- the
	// "Incidents" nav badge's data source, computed at most once per request
	// exactly like fleetStatus/fleetNodes above (task-2-brief.md's ruling:
	// "Make it request-scoped cached like the fleet memo, so each page makes
	// at most 1 call").
	incidentsFiringOnce  sync.Once
	incidentsFiringCount int
	incidentsFiringErr   error

	// activeAlertsOnce/activeAlertsCache (round-2 review finding I2) memoize
	// activeAlertsViaAPI's own apiFor(r,d).ActiveAlerts() call, computed at
	// most once per request no matter how many call sites ask for it --
	// topbarStatus (newPageData), the sidebar's Alerts badge (navCountsFor),
	// and every page's own data-builder (buildDashboardPageData,
	// buildAlertsPageData) used to each make their own independent round
	// trip; GET /alerts alone made three. Exactly the same "compute once,
	// share everywhere" fix fleetStatus/fleetNodes already got, extended
	// beyond FleetAPI to core.API.
	activeAlertsOnce  sync.Once
	activeAlertsCache []activeAlertView

	// snapshotOnce/snapshotCache/snapshotErr memoize apiFor(r,d).Snapshot()
	// the same way -- the dashboard page's own live view and the sidebar's
	// Monitoring badge (nav_counts.go's node-scope branch) used to each poll
	// a remote node's Snapshot() independently on every request scoped to
	// it. NOT used by remoteNodeSnapshot (sse.go)'s polling loop, which must
	// keep reading live rather than caching a snapshot for an SSE
	// connection's whole lifetime.
	snapshotOnce  sync.Once
	snapshotCache DashboardView
	snapshotErr   error
}

// fleetAPIFor resolves d's core.FleetAPI, collapsing a nil Deps.Fleet or a
// nil FleetAPI it returns to errNoFleetSupport -- the one place fleetMemo's
// two methods share that "no fleet wiring" check instead of duplicating it.
func fleetAPIFor(d Deps) (core.FleetAPI, error) {
	if d.Fleet == nil {
		return nil, errNoFleetSupport
	}
	fleet := d.Fleet()
	if fleet == nil {
		return nil, errNoFleetSupport
	}
	return fleet, nil
}

// fleetStatus returns d.Fleet().Status(), computed at most once for this
// memo's lifetime (one request) -- every subsequent call, from any call
// site, returns the same cached (status, err) pair without a new round
// trip.
func (m *fleetMemo) fleetStatus(d Deps) (core.FleetStatus, error) {
	m.statusOnce.Do(func() {
		fleet, err := fleetAPIFor(d)
		if err != nil {
			m.statusErr = err
			return
		}
		m.status, m.statusErr = fleet.Status()
	})
	return m.status, m.statusErr
}

// fleetNodes returns d.Fleet().Nodes(core.NodeFilter{}) -- the full,
// unfiltered roster -- computed at most once for this memo's lifetime.
// Every caller that wants a FILTERED view (handlers_fleet.go's tag/state/q
// query handling) filters this cached full roster in memory instead of
// asking Fleet() for a narrower one, so a differently-filtered request from
// a second call site in the same request still costs zero extra round
// trips.
func (m *fleetMemo) fleetNodes(d Deps) ([]core.NodeSummary, error) {
	m.nodesOnce.Do(func() {
		fleet, err := fleetAPIFor(d)
		if err != nil {
			m.nodesErr = err
			return
		}
		m.nodes, m.nodesErr = fleet.Nodes(core.NodeFilter{})
	})
	return m.nodes, m.nodesErr
}

// fleetIncidentsFiringCount returns how many fleet incidents are currently
// State=="firing" (core.IncidentFilter{State:"firing", Limit:
// fleetIncidentsFiringCap}, handlers_fleet.go), computed at most once for
// this memo's lifetime -- backs the "Incidents" nav badge (nav_counts.go's
// navCountsFor), which renders on EVERY page a fleet master serves, so this
// must never cost more than one round trip per request no matter how many
// times the badge (or some future caller) asks for it.
func (m *fleetMemo) fleetIncidentsFiringCount(d Deps) (int, error) {
	m.incidentsFiringOnce.Do(func() {
		fleet, err := fleetAPIFor(d)
		if err != nil {
			m.incidentsFiringErr = err
			return
		}
		incs, err := fleet.Incidents(core.IncidentFilter{State: "firing", Limit: fleetIncidentsFiringCap})
		if err != nil {
			m.incidentsFiringErr = err
			return
		}
		m.incidentsFiringCount = len(incs)
	})
	return m.incidentsFiringCount, m.incidentsFiringErr
}

// activeAlerts returns apiFor(r,d).ActiveAlerts(), projected the same way
// activeAlertsViaAPI (handlers_dashboard.go) always has, computed at most
// once for this memo's lifetime -- see the struct field's own doc.
// activeAlertsViaAPI itself now just calls this.
func (m *fleetMemo) activeAlerts(r *http.Request, d Deps) []activeAlertView {
	m.activeAlertsOnce.Do(func() {
		m.activeAlertsCache = fetchActiveAlertsViaAPI(r, d)
	})
	return m.activeAlertsCache
}

// snapshot returns apiFor(r,d).Snapshot(), computed at most once for this
// memo's lifetime -- see the struct field's own doc. A nil apiFor(r,d)
// reports errNoAPI rather than the zero DashboardView silently, so a caller
// that cares (buildDashboardPageData) can still tell "nothing to poll" apart
// from a genuine read error.
func (m *fleetMemo) snapshot(r *http.Request, d Deps) (DashboardView, error) {
	m.snapshotOnce.Do(func() {
		api := apiFor(r, d)
		if api == nil {
			m.snapshotErr = errNoAPI
			return
		}
		m.snapshotCache, m.snapshotErr = api.Snapshot()
	})
	return m.snapshotCache, m.snapshotErr
}

// fleetMemoFrom returns r's request-scoped *fleetMemo (attached by
// withFleetMemo, below). A request that never passed through that
// middleware -- every test that drives a handler function or
// withNodeRouter directly rather than through the full newHandler(d)
// stack -- gets a fresh, one-off memo instead: still correct (each method
// still collapses every failure mode the same way), just not memoized
// across whatever OTHER call this same test makes with a different
// *http.Request value. Production traffic always goes through
// newHandler(d), which always installs one (see routes.go).
func fleetMemoFrom(r *http.Request) *fleetMemo {
	if m, ok := r.Context().Value(fleetMemoCtxKey{}).(*fleetMemo); ok {
		return m
	}
	return &fleetMemo{}
}

// withFleetMemo installs a fresh *fleetMemo into this request's context
// before calling next, so every fleet-role/roster lookup downstream --
// withNodeRouter's own resolveMasterAndNodes call, and (for a request that
// isn't node-scoped) whatever mux handler runs next -- shares the exact
// same memo and therefore the exact same at-most-one-Status()-call/
// at-most-one-Nodes()-call budget for this request. It is cheap to wrap
// unconditionally (a single small struct allocation; nothing calls Fleet()
// until something actually asks the memo for status/nodes), so it wraps
// every request rather than only ones that turn out to need it.
//
// Wired in routes.go just outside withNodeRouter -- inside withNodeRouter
// would be too late (resolveMasterAndNodes itself needs the memo already
// present), and withNodeRouter's own request-cloning
// (r2 := r.Clone(r.Context())) carries this context value forward
// automatically, so the node-scoped re-dispatched request sees the same
// memo the original request did.
func withFleetMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), fleetMemoCtxKey{}, &fleetMemo{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
