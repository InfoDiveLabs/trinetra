package web

import (
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func f64(v float64) *float64 { return &v }

func settingsFixture(t *testing.T) *fakeManager {
	t.Helper()
	st := demoStatus()
	st.Manifest.ConfigSchema = &plugin.ConfigSchema{
		Type: "object",
		Properties: map[string]*plugin.SchemaProp{
			"endpoint": {Type: "string", Title: "Endpoint"},
			"token":    {Type: "string", Format: plugin.FormatSecret, Title: "Token"},
			"interval": {Type: "integer", Title: "Interval", Minimum: f64(5), Maximum: f64(3600)},
			"verbose":  {Type: "boolean", Title: "Verbose"},
			"mode":     {Type: "string", Title: "Mode", Enum: []string{"fast", "slow"}},
			"targets":  {Type: "array", Title: "Targets", Items: &plugin.SchemaProp{Type: "string"}},
			"warn_ms":  {Type: "integer", Title: "Warn threshold", Section: plugin.SlotAlertRules},
		},
		Order: []string{"endpoint", "token", "interval", "verbose", "mode", "targets", "warn_ms"},
	}
	m := newFakeManager(st)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	m.configs["demo"] = plugin.RedactedConfig{
		Schema: st.Manifest.ConfigSchema,
		Values: map[string]json.RawMessage{
			"endpoint": raw("https://old.example"), "token": raw("s3cr3t"), "interval": raw(30),
			"verbose": raw(true), "mode": raw("slow"), "targets": raw([]string{"a.example", "b.example"}), "warn_ms": raw(250),
		},
		SecretsSet: []string{"token"},
	}
	return m
}

func TestSettingsFormNeverEchoesSecret(t *testing.T) {
	e := newPluginEnv(t, settingsFixture(t))
	rr := e.get(RoleAdmin, "/integrations/demo")
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "s3cr3t") {
		t.Error("secret echoed into HTML")
	}
	for _, want := range []string{`type="password"`, "set — leave blank to keep", "https://old.example", `type="number"`, `min="5"`, `max="3600"`, `<select`, `<textarea`, "a.example\nb.example", `type="checkbox"`, "checked"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	m := e.m
	m.configs["demo"] = plugin.RedactedConfig{Schema: m.configs["demo"].Schema, Values: map[string]json.RawMessage{}}
	if !strings.Contains(e.get(RoleAdmin, "/integrations/demo").Body.String(), "not set") {
		t.Error("unset secret placeholder missing")
	}
}

func TestSettingsSaveKeepsSecretWhenBlank(t *testing.T) {
	m := settingsFixture(t)
	e := newPluginEnv(t, m)
	rr := e.post(RoleAdmin, "/integrations/demo/config", map[string][]string{
		"section": {"settings"}, "endpoint": {"https://new.example"}, "token": {""}, "interval": {"60"}, "mode": {"fast"}, "targets": {"x\r\ny\n"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	if len(m.setCfg) != 1 {
		t.Fatalf("SetConfig calls: %d", len(m.setCfg))
	}
	req := m.setCfg[0]
	if _, ok := req.Values["token"]; ok {
		t.Error("blank secret was submitted")
	}
	if req.Values["endpoint"] != "https://new.example" || req.Values["interval"] != "60" || req.Values["targets"] != "x\ny" {
		t.Errorf("values: %+v", req.Values)
	}
	if req.Values["verbose"] != "false" {
		t.Errorf("absent checkbox must be false: %+v", req.Values)
	}
	if _, ok := req.Values["warn_ms"]; ok {
		t.Error("alert-rules key leaked into settings save")
	}
	if req.Via != "web" || req.Actor == "" || req.ID != "demo" {
		t.Errorf("request meta: %+v", req)
	}
	e.post(RoleAdmin, "/integrations/demo/config", map[string][]string{"section": {"settings"}, "token": {"newer"}})
	if m.setCfg[1].Values["token"] != "newer" {
		t.Errorf("typed secret not sent: %+v", m.setCfg[1].Values)
	}
}

func TestSettingsShowsFieldErrors(t *testing.T) {
	m := settingsFixture(t)
	m.setCfgErr = plugin.FieldErrors{"endpoint": "must be an https URL"}
	e := newPluginEnv(t, m)
	rr := e.post(RoleAdmin, "/integrations/demo/config", map[string][]string{
		"section": {"settings"}, "endpoint": {"http://typed.example"}, "token": {"newsecret"}, "interval": {"60"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "must be an https URL") || !strings.Contains(body, "http://typed.example") {
		t.Errorf("field error or posted value missing:\n%s", body)
	}
	if strings.Contains(body, "newsecret") || strings.Contains(body, "s3cr3t") {
		t.Error("secret echoed on error re-render")
	}
}

func TestAlertRulesSectionSeparate(t *testing.T) {
	e := newPluginEnv(t, settingsFixture(t))
	body := e.get(RoleAdmin, "/integrations/demo").Body.String()
	s := strings.Index(body, `id="plugin-settings"`)
	a := strings.Index(body, `id="plugin-alert-rules"`)
	w := strings.Index(body, `name="warn_ms"`)
	ep := strings.Index(body, `name="endpoint"`)
	if s < 0 || a < 0 || w < 0 || ep < 0 {
		t.Fatalf("sections missing: %d %d %d %d", s, a, w, ep)
	}
	if !(s < ep && ep < a && a < w) {
		t.Errorf("fields not in their own sections: settings=%d endpoint=%d alerts=%d warn=%d", s, ep, a, w)
	}
}

func TestConfigAuditHasNoValues(t *testing.T) {
	m := settingsFixture(t)
	e := newPluginEnv(t, m)
	e.post(RoleAdmin, "/integrations/demo/config", map[string][]string{
		"section": {"settings"}, "endpoint": {"http://secret-host.example"}, "token": {"newsecret"},
	})
	a := readAuditActions(t, e.d.StateDir)
	if !strings.Contains(a, "plugin.config") || !strings.Contains(a, "endpoint") {
		t.Errorf("audit missing config entry: %s", a)
	}
	if strings.Contains(a, "secret-host") || strings.Contains(a, "newsecret") {
		t.Errorf("audit leaked values: %s", a)
	}
}

func TestDetailShowsPermissionsLogsAndFiles(t *testing.T) {
	st := demoStatus()
	st.Health.RecentLogs = []plugin.LogEntry{{Level: "warn", Message: "disk <b>slow</b>"}}
	st.Consents = []plugin.ConsentRecord{{Time: 1700000000, Actor: "alice", Via: "web", Version: "1.2.0", Permissions: []plugin.Permission{plugin.PermMetricsPublish}}}
	e := newPluginEnv(t, newFakeManager(st))
	body := e.get(RoleAdmin, "/integrations/demo").Body.String()
	for _, want := range []string{html.EscapeString(plugin.Describe(plugin.PermMetricsPublish)), "disk &lt;b&gt;slow&lt;/b&gt;", "alice", `href="/plugins/demo/files/report.csv"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if rr := e.get(RoleAdmin, "/integrations/nope"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown plugin status %d", rr.Code)
	}
}
