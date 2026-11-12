package web

import (
	"fmt"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

var (
	widgetTones  = map[string]bool{"": true, "ok": true, "warn": true, "crit": true}
	widgetStates = map[string]bool{"up": true, "down": true, "unknown": true}
	widgetRanges = map[string]bool{"": true, "1h": true, "6h": true, "24h": true, "7d": true}
)

// validateRender enforces widget limits and that charts and actions reference only what the plugin may use.
func validateRender(res plugin.RenderResult, st plugin.Status) error {
	if len(res.Widgets) > plugin.MaxWidgetsPerSlot {
		return fmt.Errorf("too many widgets: %d", len(res.Widgets))
	}
	for i, w := range res.Widgets {
		if err := validateWidget(w, st); err != nil {
			return fmt.Errorf("widget %d: %w", i, err)
		}
	}
	return nil
}

func validateWidget(w plugin.Widget, st plugin.Status) error {
	set := map[plugin.WidgetKind]bool{
		plugin.WidgetTimeseries: w.Timeseries != nil, plugin.WidgetStat: w.Stat != nil,
		plugin.WidgetTable: w.Table != nil, plugin.WidgetKV: w.KV != nil, plugin.WidgetLog: w.Log != nil,
		plugin.WidgetForm: w.Form != nil, plugin.WidgetAction: w.Action != nil,
		plugin.WidgetText: w.Text != nil, plugin.WidgetStatus: w.Status != nil,
	}
	known, ok := set[w.Kind]
	if !ok {
		return fmt.Errorf("unknown kind %q", w.Kind)
	}
	n := 0
	for _, v := range set {
		if v {
			n++
		}
	}
	if !known || n != 1 {
		return fmt.Errorf("kind %q needs exactly its own payload", w.Kind)
	}
	if err := checkStrings(w.Title); err != nil {
		return err
	}
	switch w.Kind {
	case plugin.WidgetTimeseries:
		t := w.Timeseries
		if len(t.Series) == 0 || len(t.Series) > plugin.MaxSeriesPerChart {
			return fmt.Errorf("timeseries needs 1-%d series", plugin.MaxSeriesPerChart)
		}
		if !widgetRanges[t.Range] {
			return fmt.Errorf("bad range %q", t.Range)
		}
		if err := checkStrings(t.Unit); err != nil {
			return err
		}
		for _, s := range t.Series {
			if err := checkStrings(s.Metric, s.Label); err != nil {
				return err
			}
			if err := allowedMetric(s.Metric, st); err != nil {
				return err
			}
		}
	case plugin.WidgetStat:
		s := w.Stat
		if !widgetTones[s.Tone] {
			return fmt.Errorf("bad tone %q", s.Tone)
		}
		return checkStrings(s.Value, s.Unit, s.Hint)
	case plugin.WidgetTable:
		t := w.Table
		if len(t.Columns) > plugin.MaxTableColumns || len(t.Rows) > plugin.MaxTableRows {
			return fmt.Errorf("table too large")
		}
		if err := checkStrings(t.Columns...); err != nil {
			return err
		}
		for _, r := range t.Rows {
			if len(r) > plugin.MaxTableColumns {
				return fmt.Errorf("table row too wide")
			}
			if err := checkStrings(r...); err != nil {
				return err
			}
		}
	case plugin.WidgetKV:
		for _, kv := range w.KV.Items {
			if err := checkStrings(kv.Key, kv.Value); err != nil {
				return err
			}
		}
	case plugin.WidgetLog:
		if len(w.Log.Lines) > plugin.MaxLogLines {
			return fmt.Errorf("too many log lines")
		}
		return checkStrings(w.Log.Lines...)
	case plugin.WidgetForm:
		f := w.Form
		if _, ok := st.Manifest.ActionDecl(f.Action); !ok {
			return fmt.Errorf("form action %q is not declared", f.Action)
		}
		if err := checkStrings(f.Action, f.Submit); err != nil {
			return err
		}
		for _, fl := range f.Fields {
			if err := checkStrings(append([]string{fl.Name, fl.Label, fl.Type, fl.Value}, fl.Options...)...); err != nil {
				return err
			}
		}
	case plugin.WidgetAction:
		if _, ok := st.Manifest.ActionDecl(w.Action.Action); !ok {
			return fmt.Errorf("action %q is not declared", w.Action.Action)
		}
		for k, v := range w.Action.Params {
			if err := checkStrings(k, v); err != nil {
				return err
			}
		}
	case plugin.WidgetText:
		if err := checkStrings(w.Text.Text); err != nil {
			return err
		}
		if f := w.Text.Format; f != "" && f != "plain" && f != "lite" {
			return fmt.Errorf("bad text format %q", f)
		}
	case plugin.WidgetStatus:
		for _, it := range w.Status.Items {
			if !widgetStates[it.State] {
				return fmt.Errorf("bad status state %q", it.State)
			}
			if err := checkStrings(it.Name, it.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}

func allowedMetric(metric string, st plugin.Status) error {
	if strings.HasPrefix(metric, "plugin.") {
		if strings.HasPrefix(metric, "plugin."+st.ID+".") {
			return nil
		}
		return fmt.Errorf("metric %q belongs to another plugin", metric)
	}
	for _, p := range st.Granted {
		if p == plugin.PermMetricsRead {
			return nil
		}
	}
	return fmt.Errorf("metric %q needs metrics.read", metric)
}

func checkStrings(ss ...string) error {
	for _, s := range ss {
		if len(s) > plugin.MaxWidgetString {
			return fmt.Errorf("string exceeds %d bytes", plugin.MaxWidgetString)
		}
	}
	return nil
}
