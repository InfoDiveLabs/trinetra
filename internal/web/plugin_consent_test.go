package web

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func unverifiedStatus() plugin.Status {
	st := demoStatus()
	st.Trust = plugin.TrustUnverified
	st.State = plugin.StateInstalled
	st.Running = false
	st.Granted = nil
	st.Manifest.Permissions = []plugin.Permission{plugin.PermNetOutbound, plugin.PermMetricsPublish}
	return st
}

func TestConsentVerifiedSingleConfirm(t *testing.T) {
	st := demoStatus()
	st.State = plugin.StateInstalled
	m := newFakeManager(st)
	e := newPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations/demo/enable").Body.String()
	if !strings.Contains(body, "Grant and enable") || strings.Contains(body, "ack_risk") || strings.Contains(body, `name="accept"`) {
		t.Errorf("verified consent page wrong:\n%s", body)
	}
	if !strings.Contains(body, html.EscapeString(plugin.Describe(plugin.PermMetricsPublish))) {
		t.Error("permission description missing")
	}
	rr := e.post(RoleAdmin, "/integrations/demo/enable", nil)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(m.enabled) != 1 {
		t.Fatalf("Enable calls: %d", len(m.enabled))
	}
	got := m.enabled[0]
	if got.ID != "demo" || got.Via != "web" || got.Actor == "" || len(got.Accepted) != 1 || got.Accepted[0] != plugin.PermMetricsPublish || got.AcknowledgedRisk {
		t.Errorf("enable request: %+v", got)
	}
}

func TestConsentUnverifiedRequiresEveryPermission(t *testing.T) {
	m := newFakeManager(unverifiedStatus())
	e := newPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations/demo/enable").Body.String()
	if !strings.Contains(body, "UNVERIFIED") || strings.Count(body, `name="accept"`) != 2 || !strings.Contains(body, `name="ack_risk"`) {
		t.Errorf("unverified consent page wrong:\n%s", body)
	}
	rr := e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"accept": {"net.outbound"}, "ack_risk": {"1"}})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rr.Code)
	}
	if len(m.enabled) != 0 {
		t.Error("Enable called with a missing permission")
	}
	rr = e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"accept": {"net.outbound", "metrics.publish", "exec"}, "ack_risk": {"1"}})
	if rr.Code != http.StatusBadRequest || len(m.enabled) != 0 {
		t.Errorf("extra permission accepted: %d", rr.Code)
	}
}

func TestConsentUnverifiedRequiresRiskAck(t *testing.T) {
	m := newFakeManager(unverifiedStatus())
	e := newPluginEnv(t, m)
	all := []string{"net.outbound", "metrics.publish"}
	if rr := e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"accept": all}); rr.Code != http.StatusBadRequest || len(m.enabled) != 0 {
		t.Errorf("enabled without ack: %d", rr.Code)
	}
	rr := e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"accept": all, "ack_risk": {"1"}})
	if rr.Code != http.StatusSeeOther || len(m.enabled) != 1 || !m.enabled[0].AcknowledgedRisk || len(m.enabled[0].Accepted) != 2 {
		t.Errorf("status %d enabled %+v", rr.Code, m.enabled)
	}
}

func TestConsentUnverifiedDisallowed(t *testing.T) {
	m := newFakeManager(unverifiedStatus())
	e := newPluginEnv(t, m)
	off := false
	e.d.Cfg = func() *config.Config {
		c := config.Default()
		c.Plugins.AllowUnverified = &off
		return c
	}
	e.h = newHandler(e.d)
	body := e.get(RoleAdmin, "/integrations/demo/enable").Body.String()
	if strings.Contains(body, `name="accept"`) || !strings.Contains(body, "allow_unverified") {
		t.Errorf("disallowed page wrong:\n%s", body)
	}
	if rr := e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"accept": {"net.outbound", "metrics.publish"}, "ack_risk": {"1"}}); rr.Code != http.StatusForbidden || len(m.enabled) != 0 {
		t.Errorf("status %d", rr.Code)
	}
}

func TestConsentPendingUpdateShowsOnlyNewPermissions(t *testing.T) {
	st := demoStatus()
	st.Pending = &plugin.PendingUpdate{
		Version:        "2.0.0",
		Manifest:       plugin.Manifest{ID: "demo", Version: "2.0.0", Permissions: []plugin.Permission{plugin.PermMetricsPublish, plugin.PermNetOutbound}},
		NewPermissions: []plugin.Permission{plugin.PermNetOutbound},
		Trust:          plugin.TrustVerified,
	}
	m := newFakeManager(st)
	e := newPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations/demo/enable?pending=1").Body.String()
	if !strings.Contains(body, html.EscapeString(plugin.Describe(plugin.PermNetOutbound))) || strings.Contains(body, html.EscapeString(plugin.Describe(plugin.PermMetricsPublish))) {
		t.Errorf("pending page must list only the new permission:\n%s", body)
	}
	if !strings.Contains(body, `name="pending" value="1"`) {
		t.Error("pending flag missing from form")
	}
	if !strings.Contains(body, `name="version" value="2.0.0"`) {
		t.Error("version binding missing from form")
	}
	e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"pending": {"1"}, "version": {"2.0.0"}})
	if len(m.enabled) != 1 || !m.enabled[0].Pending || len(plugin.Missing(m.enabled[0].Accepted, st.Pending.Manifest.Permissions)) != 0 {
		t.Errorf("pending enable must accept every permission of the new version: %+v", m.enabled)
	}
}

func TestConsentRefusedWhenVersionChangedSincePageWasShown(t *testing.T) {
	st := demoStatus()
	st.Pending = &plugin.PendingUpdate{
		Version:        "3.0.0",
		Manifest:       plugin.Manifest{ID: "demo", Version: "3.0.0", Permissions: []plugin.Permission{plugin.PermNetOutbound, plugin.PermFleetRead}},
		NewPermissions: []plugin.Permission{plugin.PermNetOutbound, plugin.PermFleetRead},
		Trust:          plugin.TrustVerified,
	}
	m := newFakeManager(st)
	e := newPluginEnv(t, m)
	rr := e.post(RoleAdmin, "/integrations/demo/enable", map[string][]string{"pending": {"1"}, "version": {"2.0.0"}})
	if rr.Code != http.StatusConflict || len(m.enabled) != 0 {
		t.Fatalf("code = %d, enabled = %+v", rr.Code, m.enabled)
	}
}

func TestEnableDisableAudited(t *testing.T) {
	st := demoStatus()
	m := newFakeManager(st)
	e := newPluginEnv(t, m)
	e.post(RoleAdmin, "/integrations/demo/enable", nil)
	e.post(RoleAdmin, "/integrations/demo/disable", nil)
	e.post(RoleAdmin, "/integrations/demo/remove", nil)
	a := readAuditActions(t, e.d.StateDir)
	for _, want := range []string{"plugin.consent", "plugin.enable", "plugin.disable", "plugin.remove"} {
		if !strings.Contains(a, want) {
			t.Errorf("audit missing %s: %s", want, a)
		}
	}
	if len(m.disabled) != 1 || len(m.removed) != 1 {
		t.Errorf("disabled %v removed %v", m.disabled, m.removed)
	}
	if rr := e.post(RoleResponder, "/integrations/demo/disable", nil); rr.Code != http.StatusForbidden {
		t.Errorf("responder disable = %d", rr.Code)
	}
}
