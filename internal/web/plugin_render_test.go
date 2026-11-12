package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func slotReq(slotID string) plugin.UIRequest {
	return plugin.UIRequest{PluginID: "demo", Request: plugin.SlotRequest{Slot: plugin.SlotNavPage, SlotID: slotID}}
}

func renderWith(t *testing.T, m *fakeManager, req plugin.UIRequest) string {
	t.Helper()
	d := Deps{Plugins: func() plugin.Manager { return m }}
	r := httptest.NewRequest(http.MethodGet, "/p/demo/main", nil)
	return string(renderSlot(context.Background(), d, r, req))
}

func TestRenderSlotErrorCardOnError(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) { return plugin.RenderResult{}, errors.New("boom") }
	out := renderWith(t, m, slotReq("main"))
	for _, want := range []string{"plugin-error", "Demo Plugin", "failed", "Details are in the plugin's log"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

func TestRenderSlotErrorCardOnTimeout(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.delay["demo/main"] = 3 * time.Second
	start := time.Now()
	out := renderWith(t, m, slotReq("main"))
	if el := time.Since(start); el >= 3*time.Second {
		t.Fatalf("render blocked for %v", el)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("want timed out card: %s", out)
	}
}

func TestRenderSlotErrorCardOnDeadlineError(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) { return plugin.RenderResult{}, plugin.ErrDeadline }
	if out := renderWith(t, m, slotReq("main")); !strings.Contains(out, "timed out") {
		t.Errorf("want timed out: %s", out)
	}
}

func TestRenderSlotNotRunning(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{}, plugin.ErrNotRunning
	}
	if out := renderWith(t, m, slotReq("main")); !strings.Contains(out, "is not running") {
		t.Errorf("want not running: %s", out)
	}
	if out := renderWith(t, newFakeManager(), slotReq("main")); !strings.Contains(out, "is not running") {
		t.Errorf("uninstalled plugin: %s", out)
	}
}

func TestRenderSlotErrorCardOnInvalidSpec(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetStat}}}, nil
	}
	if out := renderWith(t, m, slotReq("main")); !strings.Contains(out, "returned an invalid view") {
		t.Errorf("want invalid view: %s", out)
	}
}

func TestRenderSlotNeverEchoesPluginErrorText(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{}, errors.New("<b>SECRET-MSG</b>")
	}
	if out := renderWith(t, m, slotReq("main")); strings.Contains(out, "SECRET-MSG") {
		t.Errorf("plugin error text leaked: %s", out)
	}
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetStat, Stat: &plugin.StatWidget{Value: "1", Tone: "SECRET-TONE"}}}}, nil
	}
	if out := renderWith(t, m, slotReq("main")); strings.Contains(out, "SECRET-TONE") {
		t.Errorf("validation text leaked: %s", out)
	}
}

func TestRenderEachWidgetKind(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{Widgets: []plugin.Widget{
			{Kind: plugin.WidgetTimeseries, Title: "TS", Timeseries: &plugin.TimeseriesWidget{Series: []plugin.SeriesRef{{Metric: "plugin.demo.a", Label: "A"}, {Metric: "plugin.demo.b"}}, Range: "24h", Unit: "ms"}},
			{Kind: plugin.WidgetStat, Title: "ST", Stat: &plugin.StatWidget{Value: "42", Unit: "ms", Hint: "hinty", Tone: "warn"}},
			{Kind: plugin.WidgetTable, Table: &plugin.TableWidget{Columns: []string{"Col1"}, Rows: [][]string{{"<i>cell</i>"}}}},
			{Kind: plugin.WidgetKV, KV: &plugin.KVWidget{Items: []plugin.KV{{Key: "k1", Value: "v1"}}}},
			{Kind: plugin.WidgetLog, Log: &plugin.LogWidget{Lines: []string{"line-one", "line-two"}}},
			{Kind: plugin.WidgetForm, Form: &plugin.FormWidget{Action: "ping", Submit: "Go", Fields: []plugin.FormField{
				{Name: "host", Label: "Host", Type: "string", Value: "h1"},
				{Name: "mode", Label: "Mode", Type: "enum", Options: []string{"a", "b"}},
				{Name: "on", Label: "On", Type: "boolean", Value: "true"},
				{Name: "pw", Label: "Pw", Type: "secret", Value: "leaky"},
			}}},
			{Kind: plugin.WidgetAction, Action: &plugin.ActionWidget{Action: "restart", Params: map[string]string{"svc": "x"}}},
			{Kind: plugin.WidgetText, Text: &plugin.TextWidget{Text: "**bold** <u>x</u>", Format: "lite"}},
			{Kind: plugin.WidgetStatus, Status: &plugin.StatusWidget{Items: []plugin.StatusItem{{Name: "svc", State: "down", Detail: "d"}}}},
		}}, nil
	}
	out := renderWith(t, m, slotReq("main"))
	for _, want := range []string{
		`data-metrics="plugin.demo.a,plugin.demo.b"`, `data-labels="A,plugin.demo.b"`, `data-range="24h"`,
		"42", "hinty", "Col1", "&lt;i&gt;cell&lt;/i&gt;", "k1", "line-two",
		`action="/plugins/action"`, `name="csrf_token"`, `name="param.host"`, `<select`, `type="checkbox"`, `type="password"`,
		"Restart it", `data-confirm="Really restart?"`, `name="confirmed"`, `name="param.svc"`,
		"<strong>bold</strong>", "&lt;u&gt;x&lt;/u&gt;", "led crit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "leaky") {
		t.Error("secret field value echoed")
	}
	if strings.Contains(out, "<script") || strings.Contains(out, "onclick") {
		t.Error("inline script in output")
	}
}

func TestTimeseriesUsesSeriesAPI(t *testing.T) {
	m := newFakeManager(demoStatus())
	m.renderFn = func(plugin.UIRequest) (plugin.RenderResult, error) {
		return plugin.RenderResult{Widgets: []plugin.Widget{{Kind: plugin.WidgetTimeseries, Timeseries: &plugin.TimeseriesWidget{Series: []plugin.SeriesRef{{Metric: "plugin.demo.a"}}}}}}, nil
	}
	out := renderWith(t, m, slotReq("main"))
	if !strings.Contains(out, "data-plugin-chart") || !strings.Contains(out, `data-range="6h"`) {
		t.Errorf("chart container missing or range not defaulted: %s", out)
	}
	if strings.Contains(out, "<canvas") || strings.Contains(out, "<svg") {
		t.Errorf("points must come through /api/series, not inline: %s", out)
	}
}

func TestRenderSlotPassesUserAndNode(t *testing.T) {
	m := newFakeManager(demoStatus())
	d := Deps{Plugins: func() plugin.Manager { return m }}
	r := httptest.NewRequest(http.MethodGet, "/p/demo/main", nil)
	req := slotReq("main")
	req.Node = "n1"
	renderSlot(context.Background(), d, r, req)
	if len(m.renders) != 1 || m.renders[0].Node != "n1" {
		t.Errorf("node not passed: %+v", m.renders)
	}
}
