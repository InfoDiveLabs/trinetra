package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func TestIntegrationsListsInstalledWithBadges(t *testing.T) {
	ok := demoStatus()
	un := demoStatus()
	un.ID, un.Manifest.ID, un.Manifest.Name = "other", "other", "Other One"
	un.Trust = plugin.TrustUnverified
	un.State = plugin.StateRevoked
	un.RevokedReason = "found malware <b>x</b>"
	dev := demoStatus()
	dev.ID, dev.Manifest.ID, dev.Manifest.Name = "devp", "devp", "Dev Plug"
	dev.Trust = plugin.TrustDev
	e := newPluginEnv(t, newFakeManager(ok, un, dev))
	rr := e.get(RoleAdmin, "/integrations")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Demo Plugin", "Verified", "Unverified", "Dev", "Other One", "revoked", "found malware &lt;b&gt;x&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.Contains(body, `href="/integrations"`) {
		t.Error("nav entry missing")
	}
}

func TestIntegrationsUpdateAvailableBadge(t *testing.T) {
	st := demoStatus()
	st.UpdateAvailable = "2.0.0"
	e := newPluginEnv(t, newFakeManager(st))
	body := e.get(RoleAdmin, "/integrations").Body.String()
	if !strings.Contains(body, "Update available") || !strings.Contains(body, "2.0.0") || !strings.Contains(body, "/integrations/demo/update") {
		t.Errorf("update badge/button missing:\n%s", body)
	}
}

func TestIntegrationsCatalogStaleWarning(t *testing.T) {
	m := newFakeManager()
	m.catalog = plugin.Catalog{Stale: true, Error: "x", Entries: []plugin.CatalogEntry{{ID: "uptime", Name: "Uptime Checks", Publisher: "Trinetra", Description: "pings", Latest: "1.0.0"}}}
	e := newPluginEnv(t, m)
	body := e.get(RoleAdmin, "/integrations").Body.String()
	if !strings.Contains(body, "stale") || !strings.Contains(body, "Uptime Checks") {
		t.Errorf("stale warning or entry missing:\n%s", body)
	}
	m.catalog = plugin.Catalog{}
	m.catErr = plugin.ErrNoRegistryKeys
	body = e.get(RoleAdmin, "/integrations").Body.String()
	if !strings.Contains(body, "registry not configured") {
		t.Errorf("not-configured text missing:\n%s", body)
	}
}

func TestIntegrationsInstallRejectsFilePath(t *testing.T) {
	m := newFakeManager()
	e := newPluginEnv(t, m)
	for _, ref := range []string{"/etc/passwd", "./x.tar.gz", "file:///x", "../x", "http://insecure.example/p.tgz", "a/b"} {
		rr := e.post(RoleAdmin, "/integrations/install", map[string][]string{"ref": {ref}})
		if rr.Code == http.StatusSeeOther && strings.HasPrefix(rr.Header().Get("Location"), "/integrations/demo") {
			t.Errorf("%q accepted", ref)
		}
	}
	if len(m.installed) != 0 {
		t.Errorf("Install called for rejected refs: %+v", m.installed)
	}
	rr := e.post(RoleAdmin, "/integrations/install", map[string][]string{"ref": {"https://example.com/p.tar.gz"}})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/integrations/demo" {
		t.Errorf("https install: %d %s", rr.Code, rr.Header().Get("Location"))
	}
	rr = e.post(RoleAdmin, "/integrations/install", map[string][]string{"ref": {"uptime@1.0.0"}})
	if rr.Code != http.StatusSeeOther {
		t.Errorf("catalog install: %d", rr.Code)
	}
	if len(m.installed) != 2 || m.installed[0].Via != "web" || m.installed[0].Actor == "" {
		t.Errorf("install requests: %+v", m.installed)
	}
}

func TestIntegrationsAdminOnly(t *testing.T) {
	e := newPluginEnv(t, newFakeManager(demoStatus()))
	if rr := e.get(RoleViewer, "/integrations"); rr.Code != http.StatusForbidden {
		t.Errorf("viewer GET = %d", rr.Code)
	}
	if rr := e.post(RoleResponder, "/integrations/install", map[string][]string{"ref": {"uptime"}}); rr.Code != http.StatusForbidden {
		t.Errorf("responder POST = %d", rr.Code)
	}
	if len(e.m.installed) != 0 {
		t.Error("install ran for non-admin")
	}
}

func TestIntegrationsUnavailableWhenNoManager(t *testing.T) {
	e := newPluginEnv(t, nil)
	rr := e.get(RoleAdmin, "/integrations")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "unavailable") {
		t.Errorf("status %d body %s", rr.Code, rr.Body.String())
	}
}

func TestIntegrationsInstallAuditedAndUpdate(t *testing.T) {
	m := newFakeManager(demoStatus())
	e := newPluginEnv(t, m)
	e.post(RoleAdmin, "/integrations/install", map[string][]string{"ref": {"uptime"}})
	e.post(RoleAdmin, "/integrations/demo/update", nil)
	e.post(RoleAdmin, "/integrations/catalog/refresh", nil)
	if len(m.updated) != 1 || m.updated[0].ID != "demo" || m.updated[0].Via != "web" {
		t.Errorf("update: %+v", m.updated)
	}
	if m.refreshed != 1 {
		t.Errorf("refresh calls = %d", m.refreshed)
	}
	a := readAuditActions(t, e.d.StateDir)
	if !strings.Contains(a, "plugin.install") || !strings.Contains(a, "plugin.update") {
		t.Errorf("audit: %s", a)
	}
	_ = time.Now
}
