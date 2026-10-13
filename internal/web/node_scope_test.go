package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetTestDeps builds a Deps wired to api's fleet-routing fixtures the same way
// cmd/trinetra-web/main.go's buildDeps wires a real control.Client: Fleet -> api.Fleet.
func fleetTestDeps(t *testing.T, api fakeAPI) Deps {
	t.Helper()
	d := enrollTestDeps(t)
	d.API = api
	d.Fleet = api.Fleet
	d.NodeAPI = func(id string) core.API {
		n, err := api.Node(id)
		if err != nil {
			return nil
		}
		return n
	}
	return d
}

// fleetTestDepsCounting is fleetTestDeps plus a countingAPI wrapped around api as Deps.API,
// so a test can assert exactly how many core.API calls (if any) a request actually caused.
func fleetTestDepsCounting(t *testing.T, api fakeAPI) (Deps, *countingAPI) {
	t.Helper()
	d := enrollTestDeps(t)
	counting := newCountingAPI(api)
	d.API = counting
	d.Fleet = api.Fleet
	d.NodeAPI = func(id string) core.API {
		n, err := api.Node(id)
		if err != nil {
			return nil
		}
		return n
	}
	return d, counting
}

// withFakeUser attaches a *User to r's context exactly the way userMiddleware does
// (userCtxKey{}), without needing a real session/cookie round trip.
func withFakeUser(r *http.Request, role Role) *http.Request {
	u := &User{ID: "fake-user", Name: "fake", Role: role}
	return r.WithContext(context.WithValue(r.Context(), userCtxKey{}, u))
}

// nodeAwareTestMux stands in for newHandler's real route table, just enough to exercise
// withNodeRouter/nodeFrom/apiFor without depending on any real page handler's internals.
func nodeAwareTestMux(d Deps) *http.ServeMux {
	mux := http.NewServeMux()
	scopeMarkerHandler := func(w http.ResponseWriter, r *http.Request) {
		ns := nodeFrom(r)
		w.Header().Set("X-Node-Id", ns.ID)
		w.Header().Set("X-Node-Prefix", ns.Prefix)
		w.Header().Set("X-Node-Self", strconv.FormatBool(ns.Self))
		marker := ""
		if api := apiFor(r, d); api != nil {
			if mv, err := api.Monitoring(); err == nil && len(mv.FailedUnits) > 0 {
				marker = mv.FailedUnits[0]
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(marker))
	}
	mux.HandleFunc("GET /monitoring", scopeMarkerHandler)
	mux.HandleFunc("GET /events", scopeMarkerHandler)
	return mux
}

func masterFakeAPI(fleet *fakeFleet, nodes map[string]core.API) fakeAPI {
	return fakeAPI{
		monitoring: core.MonitoringView{FailedUnits: []string{"master-svc"}},
		fleet:      fleet,
		nodes:      nodes,
	}
}

func childFakeAPI(marker string) fakeAPI {
	return fakeAPI{monitoring: core.MonitoringView{FailedUnits: []string{marker}}}
}

// masterFleetWithChild is the fleet fixture ("a master fake plus a child node") most of
// this file's master-role tests share: Fleet.Nodes reports self plus child1, which alone.
func masterFleetWithChild() *fakeFleet {
	return &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Name: "master-host", Self: true, State: "up"},
			{ID: "child1", Name: "child-one", State: "up"},
		},
	}
}

// TestNodeRouterRoutesToNode pins the core plumbing: on a master, GET /n/child1/monitoring
// must reach the mux with nodeFrom(r).ID=="child1", Prefix=="/n/child1".
func TestNodeRouterRoutesToNode(t *testing.T) {
	child := childFakeAPI("child1-svc")
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), map[string]core.API{"child1": child}))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	req := withFakeUser(httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil), RoleViewer)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Node-Id"); got != "child1" {
		t.Errorf("X-Node-Id = %q, want child1", got)
	}
	if got := rr.Header().Get("X-Node-Prefix"); got != "/n/child1" {
		t.Errorf("X-Node-Prefix = %q, want /n/child1", got)
	}
	if got := rr.Header().Get("X-Node-Self"); got != "false" {
		t.Errorf("X-Node-Self = %q, want false", got)
	}
	if got := rr.Body.String(); got != "child1-svc" {
		t.Errorf("apiFor did not resolve child1's fake API: body = %q, want child1-svc", got)
	}
}

// TestNodeRouterSelfRedirects pins /n/self/... as an alternate spelling of the bare path: a
// 308 redirect (preserving the request otherwise), not a distinct page.
func TestNodeRouterSelfRedirects(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	req := withFakeUser(httptest.NewRequest(http.MethodGet, "/n/self/monitoring", nil), RoleViewer)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPermanentRedirect)
	}
	if got := rr.Header().Get("Location"); got != "/monitoring" {
		t.Errorf("Location = %q, want /monitoring", got)
	}
}

// TestNodeRouterUnknownNode404 pins the "no such node" 404: ForNode/Node never fail locally
// for an unknown id.
func TestNodeRouterUnknownNode404(t *testing.T) {
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Self: true},
			{ID: "child1"},
		},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	req := withFakeUser(httptest.NewRequest(http.MethodGet, "/n/nope/monitoring", nil), RoleViewer)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such node") {
		t.Errorf("body missing %q:\n%s", "no such node", rr.Body.String())
	}
}

// TestNodeRouterMasterLocalPaths404 pins global-constraints.md: config, channels, users,
// and public settings are master-local pages, never node-scoped.
func TestNodeRouterMasterLocalPaths404(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	for _, path := range []string{
		"/n/child1/config",
		"/n/child1/channels",
		"/n/child1/users",
		"/n/child1/settings/public",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withFakeUser(httptest.NewRequest(http.MethodGet, path, nil), RoleViewer)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestNodeRouterSoloDaemon404 pins the solo case: every /n/... path 404s, while the very
// same unprefixed route keeps working exactly as it did before node routing existed.
func TestNodeRouterSoloDaemon404(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleSolo}}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/anything/monitoring", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("/n/anything/monitoring status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}

	unprefixed := httptest.NewRecorder()
	h.ServeHTTP(unprefixed, httptest.NewRequest(http.MethodGet, "/monitoring", nil))
	if unprefixed.Code != http.StatusOK {
		t.Fatalf("/monitoring status = %d, want 200, body: %s", unprefixed.Code, unprefixed.Body.String())
	}
	if got := unprefixed.Body.String(); got != "master-svc" {
		t.Errorf("unprefixed /monitoring body = %q, want master-svc (existing route unaffected)", got)
	}
}

// TestNodeRouterOldDaemonTreatedAsSolo pins fleetRole's contract: an old daemon whose
// Fleet().Status() errors.
func TestNodeRouterOldDaemonTreatedAsSolo(t *testing.T) {
	fleet := &fakeFleet{statusErr: errNodeScopeTestOldDaemon}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if got := fleetRole(httptest.NewRequest(http.MethodGet, "/", nil), d); got != config.RoleSolo {
		t.Errorf("fleetRole = %q, want %q", got, config.RoleSolo)
	}
}

// TestNodeRouterRejectsNonGetMethod pins "remote writes are not routed".
func TestNodeRouterRejectsNonGetMethod(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))

	var reached int32
	countingHandler := func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reached, 1)
		w.WriteHeader(http.StatusOK)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /monitoring", countingHandler)
	mux.HandleFunc("POST /monitoring", countingHandler)
	mux.HandleFunc("POST /alerts/{key}/ack", countingHandler)
	mux.HandleFunc("POST /alerts/{key}/other", countingHandler)
	h := withNodeRouter(d, mux)

	for _, target := range []string{
		"/n/child1/monitoring",     // an ordinary node-scoped page, POST
		"/n/child1/alerts/k/other", // shares the /alerts/{key}/ prefix but isn't ack/unack
	} {
		t.Run(target, func(t *testing.T) {
			atomic.StoreInt32(&reached, 0)
			rr := httptest.NewRecorder()
			req := withFakeUser(httptest.NewRequest(http.MethodPost, target, nil), RoleAdmin)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
			}
			if got := atomic.LoadInt32(&reached); got != 0 {
				t.Fatalf("downstream handler was reached (%d times) -- withNodeRouter's method gate must reject this request before ever dispatching to mux", got)
			}
		})
	}

	// The one deliberate exception DOES reach downstream: proves the gate above is actually
	// discriminating on path, not just eating every non-GET/HEAD method outright.
	t.Run("/n/child1/alerts/k/ack (exception) reaches downstream", func(t *testing.T) {
		atomic.StoreInt32(&reached, 0)
		rr := httptest.NewRecorder()
		req := withFakeUser(httptest.NewRequest(http.MethodPost, "/n/child1/alerts/k/ack", nil), RoleAdmin)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (should reach the downstream counting handler), body: %s", rr.Code, rr.Body.String())
		}
		if got := atomic.LoadInt32(&reached); got != 1 {
			t.Fatalf("downstream handler reached %d times, want exactly 1", got)
		}
	})
}

// TestNodeRouterRoutesEventsStream pins that GET /n/child1/events reaches the mux with
// nodeFrom(r) resolved to child1, the same as any other GET route.
func TestNodeRouterRoutesEventsStream(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	req := withFakeUser(httptest.NewRequest(http.MethodGet, "/n/child1/events", nil), RoleViewer)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-Node-Id"); got != "child1" {
		t.Errorf("X-Node-Id = %q, want child1", got)
	}
	if got := rr.Header().Get("X-Node-Self"); got != "false" {
		t.Errorf("X-Node-Self = %q, want false", got)
	}
}

// TestNodeRouterRejectsDotDotPaths: a sub-path that tries to smuggle a ".." segment past
// the master-local/events checks -- whether written literally or percent-encoded.
func TestNodeRouterRejectsDotDotPaths(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	for _, target := range []string{
		"/n/child1/../config",
		"/n/child1/%2e%2e/config",
		"/n/child1/x/../events",
	} {
		t.Run(target, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := withFakeUser(httptest.NewRequest(http.MethodGet, target, nil), RoleViewer)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestNodeRouterBareIDRedirects: /n/{id} with no trailing slash normalizes to /n/{id}/ via
// a 308, the same way a bare directory path elsewhere in this app would.
func TestNodeRouterBareIDRedirects(t *testing.T) {
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	req := withFakeUser(httptest.NewRequest(http.MethodGet, "/n/child1", nil), RoleViewer)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d, body: %s", rr.Code, http.StatusPermanentRedirect, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/n/child1/" {
		t.Errorf("Location = %q, want /n/child1/", got)
	}
}

// TestNodeRouterAnonymousMasterRedirectsToLogin: on a master, an anonymous request to ANY
// /n/... path redirects straight to /login, before any node lookup or rendering.
func TestNodeRouterAnonymousMasterRedirectsToLogin(t *testing.T) {
	d, counting := fleetTestDepsCounting(t, masterFakeAPI(masterFleetWithChild(), map[string]core.API{"child1": childFakeAPI("child1-svc")}))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil))

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d (302 to /login), body: %s", rr.Code, http.StatusFound, rr.Body.String())
	}
	if got := rr.Header().Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}
	if n := counting.count(); n != 0 {
		t.Errorf("core.API was called %d time(s) for an anonymous master /n/... request, want 0", n)
	}
}

// TestNodeRouterAnonymousSoloNeverCallsAPI: on a solo daemon, an anonymous request to any
// /n/... path must never call into core.API either -- it's not intercepted at all.
func TestNodeRouterAnonymousSoloNeverCallsAPI(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleSolo}}
	d, counting := fleetTestDepsCounting(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/anything/monitoring", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if n := counting.count(); n != 0 {
		t.Errorf("core.API was called %d time(s) for an anonymous solo /n/... request, want 0", n)
	}
}

// TestNewHandlerWiresNodeRouter is the wiring-integration check: newHandler itself (not a
// hand-built test mux) must reach withNodeRouter.
func TestNewHandlerWiresNodeRouter(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/anything/monitoring", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNewHandlerNodeRouterAuthFlow drives the real newHandler(d) -- full middleware chain,
// real monitoringHandler/configPageHandler.
func TestNewHandlerNodeRouterAuthFlow(t *testing.T) {
	child := childFakeAPI("child1-svc")
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), map[string]core.API{"child1": child}))
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	t.Run("anonymous redirects to login", func(t *testing.T) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil))
		if rr.Code != http.StatusFound {
			t.Fatalf("status = %d, want %d, body: %s", rr.Code, http.StatusFound, rr.Body.String())
		}
		if got := rr.Header().Get("Location"); got != "/login" {
			t.Errorf("Location = %q, want /login", got)
		}
	})

	t.Run("signed-in viewer reaches the real monitoring handler", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/monitoring")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
		}
	})

	t.Run("signed-in admin still 404s a master-local page", func(t *testing.T) {
		rr := httptest.NewRecorder()
		req := seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1/config")
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
		}
	})
}

// TestDashboardRemoteNodeMemoizesSnapshotAndActiveAlerts pins that finding I2's remote-node
// case: rendering GET /n/child1/.
func TestDashboardRemoteNodeMemoizesSnapshotAndActiveAlerts(t *testing.T) {
	childCounting := newCountingAPI(childFakeAPI("child1-svc"))
	d := fleetTestDeps(t, masterFakeAPI(masterFleetWithChild(), map[string]core.API{"child1": childCounting}))
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	req := seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	if got := childCounting.snapshotCallCount(); got != 1 {
		t.Errorf("child node Snapshot() calls while rendering GET /n/child1/ = %d, want 1 (buildDashboardPageData/navCountsFor must share the request memo)", got)
	}
	if got := childCounting.activeAlertsCallCount(); got != 1 {
		t.Errorf("child node ActiveAlerts() calls while rendering GET /n/child1/ = %d, want 1 (topbarStatus/navCountsFor must share the request memo)", got)
	}
}

// errNodeScopeTestOldDaemon is a canned error standing in for the control package's real.
var errNodeScopeTestOldDaemon = errors.New("old daemon has no Fleet.Status")
