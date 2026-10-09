package web

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// errNoFleetSupport is fleetMemo's internal stand-in for "this Deps has no fleet wiring at
// all" (a nil Deps.Fleet, or a nil core.FleetAPI once called).
var errNoFleetSupport = errors.New("web: no fleet support")

// errNoAPI is fleetMemo's internal stand-in for "apiFor(r,d) returned nil" (no core.API
// wired for this request's node scope at all) -- snapshot's own caller.
var errNoAPI = errors.New("web: no api support")

// fleetMemoCtxKey is the unexported context key withFleetMemo stores this
// request's *fleetMemo under.
type fleetMemoCtxKey struct{}

// fleetMemo caches THIS request's Fleet().Status() and Fleet().Nodes(core.
type fleetMemo struct {
	statusOnce sync.Once
	status     core.FleetStatus
	statusErr  error

	nodesOnce sync.Once
	nodes     []core.NodeSummary
	nodesErr  error

	// incidentsFiring* memoize the Incidents nav badge count per request.
	incidentsFiringOnce  sync.Once
	incidentsFiringCount int
	incidentsFiringErr   error

	// activeAlertsOnce/activeAlertsCache memoize activeAlertsViaAPI's own
	// apiFor(r,d).ActiveAlerts() call.
	activeAlertsOnce  sync.Once
	activeAlertsCache []activeAlertView

	// snapshotOnce/snapshotCache/snapshotErr memoize apiFor(r,d).Snapshot() the same way, so
	// the dashboard page's own live view and the sidebar's Monitoring badge.
	snapshotOnce  sync.Once
	snapshotCache DashboardView
	snapshotErr   error
}

// fleetAPIFor resolves d's core.FleetAPI, collapsing a nil Deps.Fleet or a nil FleetAPI it
// returns to errNoFleetSupport.
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

// fleetStatus returns d.Fleet().Status(), computed at most once for this memo's lifetime
// (one request) -- every subsequent call, from any call site, returns the same cached.
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

// fleetNodes returns d.Fleet().Nodes(core.NodeFilter{}) -- the full, unfiltered roster --
// computed at most once for this memo's lifetime.
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

// fleetIncidentsFiringCount returns how many fleet incidents are currently State=="firing"
// (core.IncidentFilter{State:"firing", Limit: fleetIncidentsFiringCap}, handlers_fleet.go).
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
// activeAlertsViaAPI (handlers_dashboard.go) always has.
func (m *fleetMemo) activeAlerts(r *http.Request, d Deps) []activeAlertView {
	m.activeAlertsOnce.Do(func() {
		m.activeAlertsCache = fetchActiveAlertsViaAPI(r, d)
	})
	return m.activeAlertsCache
}

// snapshot returns apiFor(r,d).Snapshot(), computed at most once for this memo's lifetime
// -- see the struct field's own doc.
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

// fleetMemoFrom returns r's request-scoped *fleetMemo (attached by withFleetMemo, below).
func fleetMemoFrom(r *http.Request) *fleetMemo {
	if m, ok := r.Context().Value(fleetMemoCtxKey{}).(*fleetMemo); ok {
		return m
	}
	return &fleetMemo{}
}

// withFleetMemo installs a fresh *fleetMemo into this request's context before calling
// next, so every fleet-role/roster lookup downstream.
func withFleetMemo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), fleetMemoCtxKey{}, &fleetMemo{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
