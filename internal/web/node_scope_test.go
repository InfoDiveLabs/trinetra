package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// fleetTestDeps builds a Deps wired to api's fleet-routing fixtures the same
// way cmd/trinetra-web/main.go's buildDeps wires a real control.Client:
// Fleet -> api.Fleet, NodeAPI -> api.Node (falling back to nil on an unknown
// id, matching control.Client.ForNode's "never fails locally, but our fake
// resolves eagerly" shape -- see fakeAPI.Node's doc).
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

// nodeAwareTestMux stands in for newHandler's real route table, just enough
// to exercise withNodeRouter/nodeFrom/apiFor without depending on any real
// page handler's internals (this task doesn't rewire any of them -- see the
// brief's file list). GET /monitoring writes back the resolved nodeScope's
// ID/Prefix/Self as headers, plus a body marker (MonitoringView.FailedUnits'
// first entry) read through apiFor(r, d), so a test can tell "the master's
// fake API" apart from "node child1's fake API".
func nodeAwareTestMux(d Deps) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /monitoring", func(w http.ResponseWriter, r *http.Request) {
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
	})
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

// TestNodeRouterRoutesToNode pins the core plumbing: on a master, GET
// /n/child1/monitoring must reach the mux with nodeFrom(r).ID=="child1",
// Prefix=="/n/child1", and apiFor resolving to child1's own fake API (not
// the master's) -- asserted by the distinct FailedUnits payload.
func TestNodeRouterRoutesToNode(t *testing.T) {
	child := childFakeAPI("child1-svc")
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Name: "master-host", Self: true, State: "up"},
			{ID: "child1", Name: "child-one", State: "up"},
		},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, map[string]core.API{"child1": child}))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil))

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

// TestNodeRouterSelfRedirects pins /n/self/... as an alternate spelling of
// the bare path: a 308 redirect (preserving the request otherwise), not a
// distinct page.
func TestNodeRouterSelfRedirects(t *testing.T) {
	fleet := &fakeFleet{status: core.FleetStatus{Role: config.RoleMaster}}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/self/monitoring", nil))

	if rr.Code != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPermanentRedirect)
	}
	if got := rr.Header().Get("Location"); got != "/monitoring" {
		t.Errorf("Location = %q, want /monitoring", got)
	}
}

// TestNodeRouterUnknownNode404 pins the "no such node" 404: ForNode/Node
// never fail locally for an unknown id, so withNodeRouter itself must
// validate {node} against the live roster before ever dispatching.
func TestNodeRouterUnknownNode404(t *testing.T) {
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes:  []core.NodeSummary{{ID: core.SelfNodeID, Self: true}},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/nope/monitoring", nil))

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "no such node") {
		t.Errorf("body missing %q:\n%s", "no such node", rr.Body.String())
	}
}

// TestNodeRouterMasterLocalPaths404 pins global-constraints.md: config,
// channels, users, and public settings are master-local pages, never
// node-scoped, regardless of whether the node id itself would otherwise
// resolve.
func TestNodeRouterMasterLocalPaths404(t *testing.T) {
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes: []core.NodeSummary{
			{ID: core.SelfNodeID, Self: true},
			{ID: "child1"},
		},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	for _, path := range []string{
		"/n/child1/config",
		"/n/child1/channels",
		"/n/child1/users",
		"/n/child1/settings/public",
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
			}
		})
	}
}

// TestNodeRouterSoloDaemon404 pins the solo case: every /n/... path 404s
// (there's no fleet roster to resolve against), while the very same
// unprefixed route keeps working exactly as it did before node routing
// existed.
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

// TestNodeRouterOldDaemonTreatedAsSolo pins fleetRole's contract: an old
// daemon whose Fleet().Status() errors (it predates Fleet.Status entirely)
// must be treated exactly like solo, not crash or leak a 500.
func TestNodeRouterOldDaemonTreatedAsSolo(t *testing.T) {
	fleet := &fakeFleet{statusErr: errNodeScopeTestOldDaemon}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/monitoring", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
	if got := fleetRole(d); got != config.RoleSolo {
		t.Errorf("fleetRole = %q, want %q", got, config.RoleSolo)
	}
}

// TestNodeRouterRejectsNonGetMethod pins "remote writes are not routed": a
// mutation under /n/{node}/... 404s outright rather than reaching a handler
// that would try to act on a remote node.
func TestNodeRouterRejectsNonGetMethod(t *testing.T) {
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes:  []core.NodeSummary{{ID: core.SelfNodeID, Self: true}, {ID: "child1"}},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/n/child1/alerts/k/ack", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNodeRouterRejectsEventsStream pins the /events exclusion: even though
// it's GET, live event streams aren't routed to remote nodes yet.
func TestNodeRouterRejectsEventsStream(t *testing.T) {
	fleet := &fakeFleet{
		status: core.FleetStatus{Role: config.RoleMaster},
		nodes:  []core.NodeSummary{{ID: core.SelfNodeID, Self: true}, {ID: "child1"}},
	}
	d := fleetTestDeps(t, masterFakeAPI(fleet, nil))
	h := withNodeRouter(d, nodeAwareTestMux(d))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/child1/events", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// TestNewHandlerWiresNodeRouter is the wiring-integration check: newHandler
// itself (not a hand-built test mux) must reach withNodeRouter, so a plain
// Deps with no Fleet configured at all (the shape every pre-fleet test in
// this package already uses) 404s a /n/... path instead of 500ing or
// falling through to some unintended route.
func TestNewHandlerWiresNodeRouter(t *testing.T) {
	d := enrollTestDeps(t)
	h := newHandler(d)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/n/anything/monitoring", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rr.Code, rr.Body.String())
	}
}

// errNodeScopeTestOldDaemon is a canned error standing in for the control
// package's real "control: daemon does not support fleet node routing
// (upgrade trinetra)"-shaped failure an old daemon's Fleet().Status() would
// return; this test only cares that fleetRole treats ANY error this way.
var errNodeScopeTestOldDaemon = errors.New("old daemon has no Fleet.Status")
