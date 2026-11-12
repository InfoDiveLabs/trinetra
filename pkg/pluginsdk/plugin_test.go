package pluginsdk_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk"
	"github.com/InfoDiveLabs/trinetra/pkg/pluginsdk/pluginsdktest"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSDKHelloAndConfigAgainstFakeHost(t *testing.T) {
	h := pluginsdktest.New(t, "probe", []plugin.Permission{plugin.PermMetricsPublish}, map[string]any{"url": "http://x", "n": 3})
	p := pluginsdk.New(pluginsdk.Options{Version: "1.2.3"})
	h.Start(p)
	hello := p.Hello()
	if hello.PluginID != "probe" || hello.NodeID != "fake-node" || hello.HostAPI != "1.0" || len(hello.Granted) != 1 {
		t.Fatalf("%+v", hello)
	}
	cfg := p.Config()
	if cfg.String("url") != "http://x" || cfg.Int("n") != 3 || cfg.Revision != 1 {
		t.Fatalf("%+v", cfg)
	}
}

func TestSDKConfigUpdateCallsOnConfig(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, map[string]any{"n": 1})
	p := pluginsdk.New(pluginsdk.Options{})
	var mu sync.Mutex
	var seen []int64
	p.OnConfig(func(c pluginsdk.Config) {
		mu.Lock()
		seen = append(seen, c.Int("n"))
		mu.Unlock()
	})
	h.Start(p)
	h.SetConfig(map[string]any{"n": 2})
	waitFor(t, "config update", func() bool { return p.Config().Int("n") == 2 })
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != 1 || seen[len(seen)-1] != 2 {
		t.Fatalf("seen %v", seen)
	}
	if p.Config().Revision != 2 {
		t.Fatalf("revision %d", p.Config().Revision)
	}
}

func TestSDKServesRenderSlot(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	p.HandleSlot("main", func(ctx context.Context, req plugin.SlotRequest) ([]plugin.Widget, error) {
		return []plugin.Widget{pluginsdk.Stat("Up", "99", "%"), pluginsdk.Text("T", req.Context.Node)}, nil
	})
	p.HandleSlot("boom", func(context.Context, plugin.SlotRequest) ([]plugin.Widget, error) {
		panic("kaboom")
	})
	h.Start(p)
	out, err := h.Render("main", plugin.SlotContext{Node: "n1"})
	if err != nil || len(out.Widgets) != 2 || out.Widgets[0].Stat.Value != "99" || out.Widgets[1].Text.Text != "n1" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := h.Render("missing", plugin.SlotContext{}); err == nil {
		t.Fatal("unknown slot rendered")
	}
	if _, err := h.Render("boom", plugin.SlotContext{}); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("panic not contained: %v", err)
	}
	if _, err := h.Render("main", plugin.SlotContext{}); err != nil {
		t.Fatalf("plugin dead after panic: %v", err)
	}
}

func TestSDKServesInvokeAction(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	p.HandleAction("echo", func(ctx context.Context, req plugin.ActionRequest) (plugin.ActionResult, error) {
		return plugin.ActionResult{OK: true, Message: req.Params["m"] + "/" + string(req.User.Role)}, nil
	})
	p.HandleAction("fail", func(context.Context, plugin.ActionRequest) (plugin.ActionResult, error) {
		return plugin.ActionResult{}, errors.New("nope")
	})
	p.HandleValidateConfig(func(c pluginsdk.Config) plugin.FieldErrors {
		if c.String("url") == "" {
			return plugin.FieldErrors{"url": "required"}
		}
		return nil
	})
	h.Start(p)
	res, err := h.Invoke(plugin.ActionRequest{Action: "echo", Params: map[string]string{"m": "hi"}})
	if err != nil || !res.OK || res.Message != "hi/admin" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := h.Invoke(plugin.ActionRequest{Action: "fail"}); err == nil || err.Error() != "nope" {
		t.Fatalf("%v", err)
	}
	if _, err := h.Invoke(plugin.ActionRequest{Action: "ghost"}); err == nil {
		t.Fatal("unknown action ran")
	}
	var vr plugin.ValidateConfigResult
	if err := h.Call(plugin.CallValidateConfig, plugin.ValidateConfigRequest{Values: map[string]json.RawMessage{}}, &vr); err != nil || vr.Errors["url"] != "required" {
		t.Fatalf("%+v %v", vr, err)
	}
}

func TestSDKRejectsWrongCoreToken(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	p.HandleSlot("main", func(context.Context, plugin.SlotRequest) ([]plugin.Widget, error) { return nil, nil })
	h.Start(p)
	for _, tok := range []string{"wrong", ""} {
		conn, err := net.Dial("unix", h.PluginSocket())
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_ = plugin.WriteFrame(conn, plugin.Hello{Hello: plugin.PluginHelloMagic, Version: plugin.PluginProtocolVersion, Token: tok})
		var resp struct {
			Hello string `json:"hello"`
			Error string `json:"error"`
		}
		if err := plugin.ReadFrame(bufio.NewReader(conn), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Hello != "" || resp.Error == "" {
			t.Fatalf("token %q accepted: %+v", tok, resp)
		}
		conn.Close()
	}
	if _, err := h.Render("main", plugin.SlotContext{}); err != nil {
		t.Fatalf("legit call after bad tokens: %v", err)
	}
}

func TestSDKPublishAndAlert(t *testing.T) {
	h := pluginsdktest.New(t, "probe", []plugin.Permission{plugin.PermMetricsPublish, plugin.PermAlertsRaise}, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	h.Start(p)
	resp, err := p.Publish(0, map[string]float64{"a": 1, "b": 2})
	if err != nil || resp.Accepted != 2 {
		t.Fatalf("%+v %v", resp, err)
	}
	if pub := h.Published(); len(pub) != 1 || pub[0].Metrics["b"] != 2 {
		t.Fatalf("%+v", pub)
	}
	if err := p.RaiseAlert("check.down", "critical", "Down", "body"); err != nil {
		t.Fatal(err)
	}
	if err := p.ResolveAlert("check.down", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := p.ReportStatus("degraded", "slow"); err != nil {
		t.Fatal(err)
	}
	p.Logf("info", "hello %d", 5)
	a := h.Alerts()
	if len(a) != 1 || a[0].Key != "check.down" || a[0].Severity != "critical" || a[0].Title != "Down" || a[0].Body != "body" {
		t.Fatalf("%+v", a)
	}
	if r := h.Resolved(); len(r) != 1 || r[0].Body != "ok" {
		t.Fatalf("%+v", r)
	}
	if s := h.Status(); s.State != "degraded" || s.Message != "slow" {
		t.Fatalf("%+v", s)
	}
	if l := h.Logs(); len(l) != 1 || l[0].Message != "hello 5" {
		t.Fatalf("%+v", l)
	}
}

func TestSDKPublishDeniedWithoutGrant(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	h.Start(p)
	if _, err := p.Publish(0, map[string]float64{"a": 1}); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("%v", err)
	}
	if err := p.RaiseAlert("k", "info", "t", ""); err == nil {
		t.Fatal("raise allowed without grant")
	}
}

func TestSDKSeriesBuilderPrefixesNamespace(t *testing.T) {
	h := pluginsdktest.New(t, "probe", nil, nil)
	p := pluginsdk.New(pluginsdk.Options{})
	p.HandleSlot("chart", func(context.Context, plugin.SlotRequest) ([]plugin.Widget, error) {
		return []plugin.Widget{pluginsdk.Timeseries("Latency", "ms", pluginsdk.Series("latency_ms", "p50"), plugin.SeriesRef{Metric: "cpu"})}, nil
	})
	p.HandleAction("show", func(context.Context, plugin.ActionRequest) (plugin.ActionResult, error) {
		return plugin.ActionResult{OK: true, Widgets: []plugin.Widget{pluginsdk.Timeseries("x", "", pluginsdk.Series("m", ""))}}, nil
	})
	h.Start(p)
	out, err := h.Render("chart", plugin.SlotContext{})
	if err != nil {
		t.Fatal(err)
	}
	refs := out.Widgets[0].Timeseries.Series
	if refs[0].Metric != "plugin.probe.latency_ms" || refs[0].Label != "p50" || refs[1].Metric != "cpu" {
		t.Fatalf("%+v", refs)
	}
	res, err := h.Invoke(plugin.ActionRequest{Action: "show"})
	if err != nil || res.Widgets[0].Timeseries.Series[0].Metric != "plugin.probe.m" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestWidgetBuilders(t *testing.T) {
	if w := pluginsdk.StatTone(pluginsdk.Stat("a", "1", ""), "crit"); w.Stat.Tone != "crit" || w.Kind != plugin.WidgetStat {
		t.Fatalf("%+v", w)
	}
	if w := pluginsdk.Table("t", []string{"a"}, [][]string{{"1"}}); w.Kind != plugin.WidgetTable || w.Table.Rows[0][0] != "1" {
		t.Fatal("table")
	}
	if w := pluginsdk.KV("k", plugin.KV{Key: "a", Value: "b"}); w.KV.Items[0].Value != "b" {
		t.Fatal("kv")
	}
	if w := pluginsdk.Log("l", []string{"x"}); w.Log.Lines[0] != "x" {
		t.Fatal("log")
	}
	if w := pluginsdk.Status("s", plugin.StatusItem{Name: "a", State: "up"}); w.Status.Items[0].State != "up" {
		t.Fatal("status")
	}
	if w := pluginsdk.Action("restart", map[string]string{"c": "x"}); w.Action.Action != "restart" {
		t.Fatal("action")
	}
	if w := pluginsdk.Form("f", "save", "Save", plugin.FormField{Name: "a", Label: "A", Type: "string"}); w.Form.Action != "save" || w.Form.Submit != "Save" {
		t.Fatal("form")
	}
}

func TestConfigAccessors(t *testing.T) {
	c := pluginsdk.Config{Values: map[string]json.RawMessage{
		"s": json.RawMessage(`"hi"`), "i": json.RawMessage(`42`), "f": json.RawMessage(`1.5`), "b": json.RawMessage(`true`),
		"d": json.RawMessage(`"90s"`), "l": json.RawMessage(`["a","b"]`), "bad": json.RawMessage(`{}`),
	}}
	if c.String("s") != "hi" || c.Int("i") != 42 || c.Float("f") != 1.5 || !c.Bool("b") || c.Duration("d") != 90*time.Second {
		t.Fatalf("%+v", c)
	}
	if l := c.Strings("l"); len(l) != 2 || l[1] != "b" {
		t.Fatalf("%v", l)
	}
	for _, k := range []string{"missing", "bad"} {
		if c.String(k) != "" || c.Int(k) != 0 || c.Float(k) != 0 || c.Bool(k) || c.Duration(k) != 0 || c.Strings(k) != nil {
			t.Errorf("%s: non-zero from bad input", k)
		}
	}
}

func TestSDKRunFailsWithoutEnv(t *testing.T) {
	p := pluginsdk.New(pluginsdk.Options{})
	p.SetEnv(func(string) string { return "" })
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded without environment")
	}
}

func TestSDKIsStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	const mod = "github.com/InfoDiveLabs/trinetra/"
	allowed := map[string]bool{
		mod + "internal/plugin": true, mod + "internal/core": true, mod + "internal/config": true, mod + "internal/update": true,
		mod + "pkg/pluginsdk": true, mod + "pkg/pluginsdk/pluginsdktest": true,
	}
	for _, line := range strings.Fields(string(out)) {
		first := strings.SplitN(line, "/", 2)[0]
		if !strings.Contains(first, ".") {
			continue
		}
		if !allowed[line] {
			t.Errorf("non-stdlib dependency: %s", line)
		}
	}
}
