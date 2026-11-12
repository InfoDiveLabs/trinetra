package web

import (
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

func testStatus(perms ...plugin.Permission) plugin.Status {
	var st plugin.Status
	st.ID = "demo"
	st.Granted = perms
	st.Manifest = plugin.Manifest{
		ID: "demo", Name: "Demo",
		Contributes: plugin.Contributes{Actions: []plugin.ActionDecl{{ID: "restart", Label: "Restart", MinRole: plugin.RoleAdmin, Mutating: true}}},
	}
	return st
}

func statW(v string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetStat, Stat: &plugin.StatWidget{Value: v}}
}

func TestValidateRenderLimits(t *testing.T) {
	st := testStatus()
	var many []plugin.Widget
	for i := 0; i <= plugin.MaxWidgetsPerSlot; i++ {
		many = append(many, statW("1"))
	}
	if err := validateRender(plugin.RenderResult{Widgets: many}, st); err == nil {
		t.Error("too many widgets accepted")
	}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{statW(strings.Repeat("x", plugin.MaxWidgetString+1))}}, st); err == nil {
		t.Error("oversize string accepted")
	}
	rows := make([][]string, plugin.MaxTableRows+1)
	tw := plugin.Widget{Kind: plugin.WidgetTable, Table: &plugin.TableWidget{Columns: []string{"a"}, Rows: rows}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tw}}, st); err == nil {
		t.Error("too many rows accepted")
	}
	cols := make([]string, plugin.MaxTableColumns+1)
	tw = plugin.Widget{Kind: plugin.WidgetTable, Table: &plugin.TableWidget{Columns: cols}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tw}}, st); err == nil {
		t.Error("too many columns accepted")
	}
	lw := plugin.Widget{Kind: plugin.WidgetLog, Log: &plugin.LogWidget{Lines: make([]string, plugin.MaxLogLines+1)}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{lw}}, st); err == nil {
		t.Error("too many log lines accepted")
	}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{statW("ok")}}, st); err != nil {
		t.Errorf("valid render rejected: %v", err)
	}
	badTone := plugin.Widget{Kind: plugin.WidgetStat, Stat: &plugin.StatWidget{Value: "1", Tone: "purple"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{badTone}}, st); err == nil {
		t.Error("bad tone accepted")
	}
	badState := plugin.Widget{Kind: plugin.WidgetStatus, Status: &plugin.StatusWidget{Items: []plugin.StatusItem{{Name: "a", State: "maybe"}}}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{badState}}, st); err == nil {
		t.Error("bad status state accepted")
	}
	badRange := plugin.Widget{Kind: plugin.WidgetTimeseries, Timeseries: &plugin.TimeseriesWidget{Series: []plugin.SeriesRef{{Metric: "plugin.demo.x"}}, Range: "3d"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{badRange}}, st); err == nil {
		t.Error("bad range accepted")
	}
}

func tsW(metric string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetTimeseries, Timeseries: &plugin.TimeseriesWidget{Series: []plugin.SeriesRef{{Metric: metric}}}}
}

func TestValidateRenderRejectsForeignMetric(t *testing.T) {
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tsW("plugin.other.x")}}, testStatus()); err == nil {
		t.Error("foreign plugin metric accepted")
	}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tsW("cpu")}}, testStatus()); err == nil {
		t.Error("core metric accepted without metrics.read")
	}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tsW("plugin.demo.x")}}, testStatus()); err != nil {
		t.Errorf("own metric rejected: %v", err)
	}
}

func TestValidateRenderAllowsCoreMetricWithMetricsRead(t *testing.T) {
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tsW("cpu")}}, testStatus(plugin.PermMetricsRead)); err != nil {
		t.Errorf("core metric rejected with metrics.read: %v", err)
	}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{tsW("plugin.other.x")}}, testStatus(plugin.PermMetricsRead)); err == nil {
		t.Error("foreign plugin metric accepted even with metrics.read")
	}
}

func TestValidateRenderRejectsUndeclaredAction(t *testing.T) {
	st := testStatus()
	a := plugin.Widget{Kind: plugin.WidgetAction, Action: &plugin.ActionWidget{Action: "nuke"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{a}}, st); err == nil {
		t.Error("undeclared action accepted")
	}
	f := plugin.Widget{Kind: plugin.WidgetForm, Form: &plugin.FormWidget{Action: "nuke"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{f}}, st); err == nil {
		t.Error("undeclared form action accepted")
	}
	ok := plugin.Widget{Kind: plugin.WidgetAction, Action: &plugin.ActionWidget{Action: "restart"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{ok}}, st); err != nil {
		t.Errorf("declared action rejected: %v", err)
	}
}

func TestValidateRenderExactlyOnePayload(t *testing.T) {
	st := testStatus()
	none := plugin.Widget{Kind: plugin.WidgetStat}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{none}}, st); err == nil {
		t.Error("missing payload accepted")
	}
	two := statW("1")
	two.Text = &plugin.TextWidget{Text: "x"}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{two}}, st); err == nil {
		t.Error("two payloads accepted")
	}
	unk := plugin.Widget{Kind: "iframe", Stat: &plugin.StatWidget{Value: "1"}}
	if err := validateRender(plugin.RenderResult{Widgets: []plugin.Widget{unk}}, st); err == nil {
		t.Error("unknown kind accepted")
	}
}
