package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk/pluginsdktest"
)

func loadManifest(t *testing.T) plugin.Manifest {
	t.Helper()
	b, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("0", 64)
	b = []byte(strings.Replace(string(b), `"binaries": []`, `"binaries": [{"os":"linux","arch":"amd64","sha256":"`+sum+`","size":1}]`, 1))
	m, err := plugin.DecodeManifest(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManifestValid(t *testing.T) {
	m := loadManifest(t)
	if m.ID != "prometheus" || m.HostAPI != "^1.0" {
		t.Fatalf("id %q host_api %q", m.ID, m.HostAPI)
	}
	want := map[plugin.Permission]bool{plugin.PermMetricsRead: true, plugin.PermNetListen: true, plugin.PermFleetRead: true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions %v", m.Permissions)
	}
	for _, p := range m.Permissions {
		if !want[p] {
			t.Errorf("unexpected permission %s", p)
		}
	}
	if _, ok := m.SlotDecl(plugin.SlotSettingsForm, "settings"); !ok {
		t.Error("settings.form/settings slot missing")
	}
	if len(m.Contributes.Files) != 1 || m.Contributes.Files[0].Name != "grafana-dashboard.json" {
		t.Errorf("files %+v", m.Contributes.Files)
	}
	if st, err := os.Stat("files/grafana-dashboard.json"); err != nil || st.Size() == 0 {
		t.Errorf("grafana-dashboard.json: %v", err)
	}
}

func TestManifestDefaults(t *testing.T) {
	m := loadManifest(t)
	vals := plugin.WithDefaults(m.ConfigSchema, nil)
	if got := string(vals["listen"]); got != `"127.0.0.1:9469"` {
		t.Errorf("listen default %s", got)
	}
	if got := string(vals["include_fleet"]); got != "false" {
		t.Errorf("include_fleet default %s", got)
	}
	if errs := plugin.ValidateValues(m.ConfigSchema, map[string]json.RawMessage{"listen": json.RawMessage(`"not-an-address"`)}); errs["listen"] == "" {
		t.Error("schema accepted a listen value without a port")
	}
	if errs := plugin.ValidateValues(m.ConfigSchema, map[string]json.RawMessage{"listen": json.RawMessage(`"0.0.0.0:9469"`)}); errs != nil {
		t.Errorf("schema rejected a valid listen: %v", errs)
	}
}

func TestAgainstFakeHost(t *testing.T) {
	m := loadManifest(t)
	h := pluginsdktest.New(t, "prometheus", m.Permissions, map[string]any{"listen": "127.0.0.1:0", "bearer_token": "tok", "include_fleet": true})
	h.SetSnapshot(testView())
	h.SetFleetNodes([]core.NodeSummary{{ID: "n1", Name: "web", State: "online"}})
	p, exp := newPlugin()
	t.Cleanup(exp.close)
	h.Start(p)

	waitFor(t, "status ok", func() bool { return h.Status().State == "ok" })
	addr := exp.addr()
	if want := "serving on " + addr; h.Status().Message != want {
		t.Fatalf("status message %q, want %q", h.Status().Message, want)
	}
	if code, _ := get(t, "http://"+addr+"/metrics", ""); code != 401 {
		t.Fatalf("unauthenticated status %d", code)
	}
	code, body := get(t, "http://"+addr+"/metrics", "Bearer tok")
	if code != 200 || !strings.Contains(body, "trinetra_cpu_usage_percent 12.5\n") || !strings.Contains(body, `trinetra_node_up{node="web",id="n1"} 1`) {
		t.Fatalf("status %d body:\n%s", code, body)
	}

	h.SetConfig(map[string]any{"listen": "127.0.0.1:0", "bearer_token": "", "include_fleet": false})
	waitFor(t, "token cleared", func() bool { return exp.settings().token == "" })
	_, body = get(t, "http://"+addr+"/metrics", "")
	if strings.Contains(body, "trinetra_node_up") {
		t.Fatalf("fleet metrics still served:\n%s", body)
	}
}

func TestValidateConfigRejectsBadListen(t *testing.T) {
	m := loadManifest(t)
	h := pluginsdktest.New(t, "prometheus", m.Permissions, map[string]any{"listen": "127.0.0.1:0"})
	p, exp := newPlugin()
	t.Cleanup(exp.close)
	h.Start(p)
	var res plugin.ValidateConfigResult
	err := h.Call(plugin.CallValidateConfig, plugin.ValidateConfigRequest{Values: map[string]json.RawMessage{"listen": json.RawMessage(`"localhost"`)}}, &res)
	if err != nil || res.Errors["listen"] == "" {
		t.Fatalf("errors %v err %v", res.Errors, err)
	}
	res = plugin.ValidateConfigResult{}
	err = h.Call(plugin.CallValidateConfig, plugin.ValidateConfigRequest{Values: map[string]json.RawMessage{"listen": json.RawMessage(`"127.0.0.1:9469"`)}}, &res)
	if err != nil || len(res.Errors) != 0 {
		t.Fatalf("valid listen rejected: %v %v", res.Errors, err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
