package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// nodeScopedDeps builds a master Deps (fleetTestDeps, node_scope_test.go's
// fixture wiring) with master as the daemon's own core.API and child
// registered as fleet node "child1" (masterFleetWithChild's roster) -- the
// shared fixture every test in this file uses to assert a /n/child1/...
// page renders child's data, never master's.
func nodeScopedDeps(t *testing.T, master, child fakeAPI) Deps {
	t.Helper()
	master.fleet = masterFleetWithChild()
	master.nodes = map[string]core.API{"child1": child}
	return fleetTestDeps(t, master)
}

// TestNodeScopedDashboardShowsChildData pins that GET /n/child1/ (the
// dashboard, "/{$}") renders child1's own live snapshot and host info, not
// the master's -- Task 2's core obligation for handlers_dashboard.go.
func TestNodeScopedDashboardShowsChildData(t *testing.T) {
	master := fakeAPI{
		snap:     core.DashboardView{TopCPUContainers: []core.ContainerView{{Name: "master-only-svc", State: "running"}}},
		hostInfo: core.HostInfoView{OS: "MasterOS 1.0"},
	}
	child := fakeAPI{
		snap:     core.DashboardView{TopCPUContainers: []core.ContainerView{{Name: "child1-only-svc", State: "running"}}},
		hostInfo: core.HostInfoView{OS: "ChildOS 2.0"},
	}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "child1-only-svc") {
		t.Errorf("dashboard missing child1's container:\n%s", body)
	}
	if strings.Contains(body, "master-only-svc") {
		t.Errorf("dashboard leaked master's container onto a node-scoped page:\n%s", body)
	}
	if !strings.Contains(body, "ChildOS 2.0") {
		t.Errorf("dashboard host strip missing child1's OS:\n%s", body)
	}
	if strings.Contains(body, "MasterOS 1.0") {
		t.Errorf("dashboard host strip leaked master's OS onto a node-scoped page:\n%s", body)
	}
}

// TestNodeScopedMonitoringShowsChildData pins /n/child1/monitoring
// (handlers_monitoring.go).
func TestNodeScopedMonitoringShowsChildData(t *testing.T) {
	master := fakeAPI{monitoring: core.MonitoringView{FailedUnits: []string{"master-broken.service"}}}
	child := fakeAPI{monitoring: core.MonitoringView{FailedUnits: []string{"child1-broken.service"}}}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "child1-broken.service") {
		t.Errorf("monitoring missing child1's failed unit:\n%s", body)
	}
	if strings.Contains(body, "master-broken.service") {
		t.Errorf("monitoring leaked master's failed unit onto a node-scoped page:\n%s", body)
	}
}

// TestNodeScopedHostShowsChildData pins /n/child1/host (handlers_host.go).
func TestNodeScopedHostShowsChildData(t *testing.T) {
	master := fakeAPI{hostInfo: core.HostInfoView{Hostname: "master-host"}}
	child := fakeAPI{hostInfo: core.HostInfoView{Hostname: "child1-host"}}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/host"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "child1-host") {
		t.Errorf("host page missing child1's hostname:\n%s", body)
	}
	if strings.Contains(body, "master-host") {
		t.Errorf("host page leaked master's hostname onto a node-scoped page:\n%s", body)
	}
}

// TestNodeScopedHistoryPageShowsChildDiskMounts pins that /n/child1/history's
// disk panel graphs child1's own mounts (apiFor(r,d).Snapshot(), not the
// master-only Deps.Snapshot() closure -- see historyDiskMounts's doc).
func TestNodeScopedHistoryPageShowsChildDiskMounts(t *testing.T) {
	master := fakeAPI{}
	child := fakeAPI{snap: core.DashboardView{Disks: []core.DiskView{{Mount: "/child1-only-mount"}}}}
	d := nodeScopedDeps(t, master, child)
	d.Snapshot = func() DashboardView { return DashboardView{Disks: []DiskView{{Mount: "/master-only-mount"}}} }
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/history"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "disk:/child1-only-mount") {
		t.Errorf("history page missing child1's disk mount:\n%s", body)
	}
	if strings.Contains(body, "disk:/master-only-mount") {
		t.Errorf("history page leaked master's Deps.Snapshot() disk mount onto a node-scoped page:\n%s", body)
	}
}

// TestNodeScopedSeriesAPIShowsChildData pins GET /n/child1/api/series.
func TestNodeScopedSeriesAPIShowsChildData(t *testing.T) {
	master := fakeAPI{series: map[string][]core.SeriesPoint{"cpu": {{TS: 1000, Avg: 11}}}}
	child := fakeAPI{series: map[string][]core.SeriesPoint{"cpu": {{TS: 1000, Avg: 77}}}}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/api/series?metric=cpu&from=500&to=1500"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp seriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Series) < 2 || len(resp.Series[1]) != 1 || resp.Series[1][0] != 77 {
		t.Errorf("series avg = %+v, want [77] (child1's data)", resp.Series)
	}
}

// TestNodeScopedDowntimeAPIShowsChildData pins GET /n/child1/api/downtime.
func TestNodeScopedDowntimeAPIShowsChildData(t *testing.T) {
	master := fakeAPI{events: []core.DownEventView{{Type: "power_down", Start: 1, End: 2, DurationSec: 1}}}
	child := fakeAPI{events: []core.DownEventView{{Type: "net_down", Start: 10, End: 70, DurationSec: 60}}}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/api/downtime?from=0&to=100"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	var resp downtimeResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if len(resp.Events) != 1 || resp.Events[0].Type != "net_down" {
		t.Errorf("events = %+v, want child1's single net_down event", resp.Events)
	}
}

// TestNodeScopedAlertsPageShowsChildDataAndDisablesAck pins the alerts page's
// node-scope behavior: active alerts + history come from child1, and (per
// global-constraints.md / the plan's ruling) the Ack action for an unacked
// active alert renders disabled with the exact reason text and no POST form
// action, since alert ack isn't routed to a remote node.
func TestNodeScopedAlertsPageShowsChildDataAndDisablesAck(t *testing.T) {
	master := fakeAPI{active: []core.AlertRecord{{Key: "master-only", Time: 1, Source: "master alert"}}}
	child := fakeAPI{
		active:  []core.AlertRecord{{Key: "child1-only", Time: 2, Source: "child1 alert"}},
		history: []core.AlertRecord{{Time: 2, Key: "child1-only", Title: "child1 fired", Kind: "fire", Source: "child1 alert"}},
	}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1/alerts"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "child1 alert") {
		t.Errorf("alerts page missing child1's active alert:\n%s", body)
	}
	if !strings.Contains(body, "child1 fired") {
		t.Errorf("alerts page missing child1's alert history:\n%s", body)
	}
	if strings.Contains(body, "master alert") {
		t.Errorf("alerts page leaked master's active alert onto a node-scoped page:\n%s", body)
	}
	if !strings.Contains(body, "not available for a remote node yet") {
		t.Errorf("alerts page missing the disabled-ack reason text:\n%s", body)
	}
	if strings.Contains(body, `action="/alerts/`) {
		t.Errorf("alerts page still renders a live Ack form action on a remote-node page:\n%s", body)
	}
	if strings.Contains(body, ">Ack<") {
		t.Errorf("alerts page still renders an enabled Ack button on a remote-node page:\n%s", body)
	}
}

// TestNodeScopedContainerLogsReturns409 pins the ruling: GET
// /n/child1/api/container/logs never reaches the daemon at all -- it
// returns 409 with the fixed JSON body {"error":"not available for a
// remote node yet"} regardless of what the underlying fake API would say.
func TestNodeScopedContainerLogsReturns409(t *testing.T) {
	master := fakeAPI{}
	child := fakeAPI{containerLogs: "should never be returned"}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1/api/container/logs?name=web"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	if resp["error"] != "not available for a remote node yet" {
		t.Errorf(`error = %q, want "not available for a remote node yet"`, resp["error"])
	}
	if strings.Contains(rr.Body.String(), "should never be returned") {
		t.Errorf("container logs handler called through to the fake API for a remote node:\n%s", rr.Body.String())
	}
}

// TestNodeScopedTopbarAndAlertBadgeReflectNode pins that a node-scoped
// page's topbar status pill, sidebar Alerts badge, and CoreVersion footer
// all reflect child1's own state, not the master's -- newPageData/
// navCountsFor/coreVersionViaAPI all read through apiFor(r, d).
func TestNodeScopedTopbarAndAlertBadgeReflectNode(t *testing.T) {
	master := fakeAPI{version: "master-v1"} // no active alerts on master
	child := fakeAPI{
		active:  []core.AlertRecord{{Key: "child1-crit", Time: 1, Source: "child1 crit", Severity: "critical"}},
		version: "child1-v2",
	}
	d := nodeScopedDeps(t, master, child)
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleViewer, http.MethodGet, "/n/child1/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "led crit") {
		t.Errorf("node-scoped topbar missing crit led class for child1's active critical alert:\n%s", body)
	}
	if !strings.Contains(body, "1 alert firing") {
		t.Errorf("node-scoped topbar missing real alert-firing text:\n%s", body)
	}
	if strings.Contains(body, "All systems normal") {
		t.Errorf("node-scoped topbar shows master's ok status instead of child1's firing alert:\n%s", body)
	}
	if !strings.Contains(body, `Alerts<span class="ct">1</span>`) {
		t.Errorf("node-scoped sidebar Alerts badge missing child1's count of 1:\n%s", body)
	}
	if !strings.Contains(body, "core child1-v2") {
		t.Errorf("node-scoped sidebar footer missing child1's core version:\n%s", body)
	}
	if strings.Contains(body, "core master-v1") {
		t.Errorf("node-scoped sidebar footer shows master's core version instead of child1's:\n%s", body)
	}
}

// TestNodeScopedChannelsAndUsersBadgesStayMasterLocal pins the ruling: on a
// remote node page, the Channels and Users nav badges stay the master's own
// values (they're master-local concepts), while Monitoring (a daemon
// concept) follows the node scope.
func TestNodeScopedChannelsAndUsersBadgesStayMasterLocal(t *testing.T) {
	master := fakeAPI{snap: core.DashboardView{ContainersTotal: 3}}
	child := fakeAPI{snap: core.DashboardView{ContainersTotal: 9}}
	d := nodeScopedDeps(t, master, child)
	d.Cfg = func() *config.Config {
		cfg := config.Default()
		cfg.Channels = []config.ChannelConfig{
			{Name: "tg", Type: "telegram", Enabled: true},
			{Name: "mail", Type: "email", Enabled: true},
		}
		return cfg
	}
	h := newHandler(d)
	users := newUserStore(d.StateDir)
	sessions := newSessionStore(d.StateDir)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, seedSignedInRequest(t, users, sessions, RoleAdmin, http.MethodGet, "/n/child1/monitoring"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `Channels<span class="ct">2</span>`) {
		t.Errorf("node-scoped Channels badge should stay the master's own count of 2:\n%s", body)
	}
	if !strings.Contains(body, `Monitoring<span class="ct">9</span>`) {
		t.Errorf("node-scoped Monitoring badge should reflect child1's container count of 9:\n%s", body)
	}
	if strings.Contains(body, `Monitoring<span class="ct">3</span>`) {
		t.Errorf("node-scoped Monitoring badge leaked master's container count:\n%s", body)
	}
}
