package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func masterPluginEnv(t *testing.T, m *fakeManager) *pluginEnv {
	t.Helper()
	d := fleetAdminDeps(t, &fakeFleet{})
	d.Plugins = func() plugin.Manager { return m }
	return &pluginEnv{t: t, d: d, h: newHandler(d), m: m, users: newUserStore(d.StateDir), sessions: newSessionStore(d.StateDir)}
}

func TestPushFormMasterOnly(t *testing.T) {
	m := newFakeManager(demoStatus())
	solo := newPluginEnv(t, m)
	if body := solo.get(RoleAdmin, "/integrations/demo").Body.String(); strings.Contains(body, "Push to nodes") {
		t.Error("push form shown on a non-master")
	}
	if rr := solo.post(RoleAdmin, "/integrations/demo/push", map[string][]string{"node": {"web1"}}); rr.Code != http.StatusNotFound || len(m.pushed) != 0 {
		t.Errorf("non-master push = %d", rr.Code)
	}

	e := masterPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations/demo").Body.String()
	if !strings.Contains(body, "Push to nodes") || !strings.Contains(body, `name="node" value="web1"`) || !strings.Contains(body, `<option value="web">`) {
		t.Errorf("master push form wrong:\n%s", body)
	}
	if strings.Contains(body, `value="self"`) {
		t.Error("self offered as a push target")
	}
	m.nodes = []plugin.NodeState{{Node: "web1", NodeName: "web1", Phase: "installed", Version: "1.2.0"}}
	if body := e.get(RoleAdmin, "/integrations/demo").Body.String(); !strings.Contains(body, "installed") {
		t.Error("node state table missing")
	}
	rr := e.post(RoleAdmin, "/integrations/demo/push", map[string][]string{"node": {"web1", "web2"}})
	if rr.Code != http.StatusSeeOther || len(m.pushed) != 1 || len(m.pushed[0].Nodes) != 2 || m.pushed[0].Via != "web" {
		t.Fatalf("push: %d %+v", rr.Code, m.pushed)
	}
	if !strings.Contains(readAuditActions(t, e.d.StateDir), "plugin.push") {
		t.Error("push not audited")
	}
	if rr := e.post(RoleAdmin, "/integrations/demo/push", nil); rr.Code != http.StatusBadRequest {
		t.Errorf("push with no target = %d", rr.Code)
	}
}

func TestPushUnverifiedShowsNodesAndRequiresEveryPermission(t *testing.T) {
	m := newFakeManager(unverifiedStatus())
	e := masterPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations/demo").Body.String()
	if !strings.Contains(body, "UNVERIFIED") || !strings.Contains(body, `name="node" value="web2"`) || strings.Count(body, `name="accept"`) != 2 || !strings.Contains(body, `name="ack_risk"`) {
		t.Errorf("unverified push form wrong")
	}
	nodes := []string{"web1"}
	if rr := e.post(RoleAdmin, "/integrations/demo/push", map[string][]string{"node": nodes, "accept": {"net.outbound"}, "ack_risk": {"1"}}); rr.Code != http.StatusBadRequest {
		t.Errorf("partial accept = %d", rr.Code)
	}
	if rr := e.post(RoleAdmin, "/integrations/demo/push", map[string][]string{"node": nodes, "accept": {"net.outbound", "metrics.publish"}}); rr.Code != http.StatusBadRequest {
		t.Errorf("no ack = %d", rr.Code)
	}
	if len(m.pushed) != 0 {
		t.Fatal("push ran without full consent")
	}
	rr := e.post(RoleAdmin, "/integrations/demo/push", map[string][]string{"node": nodes, "accept": {"net.outbound", "metrics.publish"}, "ack_risk": {"1"}})
	if rr.Code != http.StatusSeeOther || len(m.pushed) != 1 || !m.pushed[0].AcknowledgedRisk || len(m.pushed[0].Accepted) != 2 {
		t.Errorf("full consent push: %d %+v", rr.Code, m.pushed)
	}
}

func TestNodeScopedSlotPassesNode(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.offers = []plugin.SlotOffer{offer("demo", plugin.SlotHostSection, "hs", plugin.RoleViewer)}
	m.renderFn = func(r plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{Widgets: []plugin.Widget{statW("REMOTEVAL")}}, nil
	}
	e := masterPluginEnv(t, m)
	rr := e.get(RoleViewer, "/n/web1/plugins/slot?slot=host.section&plugin=demo&slot_id=hs")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "REMOTEVAL") {
		t.Fatalf("remote slot: %d %s", rr.Code, rr.Body.String())
	}
	if len(m.renders) != 1 || m.renders[0].Node != "web1" || m.renders[0].Request.Context.Node != "web1" {
		t.Errorf("node not passed: %+v", m.renders)
	}
	found := false
	for _, n := range m.slotNodes {
		found = found || n == "web1"
	}
	if !found {
		t.Errorf("Slots not scoped to node: %v", m.slotNodes)
	}
	m.renders = nil
	e.get(RoleViewer, "/plugins/slot?slot=host.section&plugin=demo&slot_id=hs")
	if len(m.renders) != 1 || m.renders[0].Node != "" {
		t.Errorf("self scope must not carry a node: %+v", m.renders)
	}
}

func TestNodeScopedSlotNotInstalledOnMaster(t *testing.T) {
	m := newFakeManager()
	m.offers = []plugin.SlotOffer{offer("ghost", plugin.SlotHostSection, "hs", plugin.RoleViewer)}
	e := masterPluginEnv(t, m)
	rr := e.get(RoleViewer, "/n/web1/plugins/slot?slot=host.section&plugin=ghost&slot_id=hs")
	if !strings.Contains(rr.Body.String(), "is not running") {
		t.Errorf("want not-running card: %s", rr.Body.String())
	}
}

func TestFleetAdminShowsPluginStates(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.nodes = []plugin.NodeState{{Node: "web1", NodeName: "web1", Phase: "downloading", Version: "1.2.0", Received: 100, Size: 400}, {Node: "web2", NodeName: "web2", Phase: "failed", Error: "disk full"}}
	e := masterPluginEnv(t, m)
	req := seedSignedInRequest(t, e.users, e.sessions, RoleAdmin, http.MethodGet, "/fleet/admin")
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	body := rr.Body.String()
	for _, want := range []string{"Plugins", "Demo Plugin", "downloading", "disk full"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}
