package pluginsdk

import "github.com/InfoDiveLabs/trinetra/internal/plugin"

const localMarker = "pluginLocal:"

// Stat is a single-value tile.
func Stat(title, value, unit string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetStat, Title: title, Stat: &plugin.StatWidget{Value: value, Unit: unit}}
}

// StatTone returns w, a stat widget, with its tone set to ok, warn or crit.
func StatTone(w plugin.Widget, tone string) plugin.Widget {
	if w.Stat != nil {
		s := *w.Stat
		s.Tone = tone
		w.Stat = &s
	}
	return w
}

// Timeseries is a chart of one or more series.
func Timeseries(title, unit string, series ...plugin.SeriesRef) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetTimeseries, Title: title, Timeseries: &plugin.TimeseriesWidget{Series: series, Unit: unit}}
}

// Series references one of the plugin's own metrics by its plugin-local name; the SDK adds the plugin.<id>. prefix when a slot is rendered.
func Series(metric, label string) plugin.SeriesRef {
	return plugin.SeriesRef{Metric: localMarker + metric, Label: label}
}

// Table is a grid of strings.
func Table(title string, columns []string, rows [][]string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetTable, Title: title, Table: &plugin.TableWidget{Columns: columns, Rows: rows}}
}

// KV is a key/value list.
func KV(title string, kv ...plugin.KV) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetKV, Title: title, KV: &plugin.KVWidget{Items: kv}}
}

// Log is a block of log lines.
func Log(title string, lines []string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetLog, Title: title, Log: &plugin.LogWidget{Lines: lines}}
}

// Text is a plain-text paragraph.
func Text(title, text string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetText, Title: title, Text: &plugin.TextWidget{Text: text, Format: "plain"}}
}

// Status is a list of named up/down items.
func Status(title string, items ...plugin.StatusItem) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetStatus, Title: title, Status: &plugin.StatusWidget{Items: items}}
}

// Action is a button that invokes a declared action.
func Action(actionID string, params map[string]string) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetAction, Action: &plugin.ActionWidget{Action: actionID, Params: params}}
}

// Form is an input form submitted to a declared action.
func Form(title, actionID, submit string, fields ...plugin.FormField) plugin.Widget {
	return plugin.Widget{Kind: plugin.WidgetForm, Title: title, Form: &plugin.FormWidget{Action: actionID, Fields: fields, Submit: submit}}
}

func resolveLocal(ws []plugin.Widget, pluginID string) []plugin.Widget {
	for i := range ws {
		t := ws[i].Timeseries
		if t == nil {
			continue
		}
		refs := make([]plugin.SeriesRef, len(t.Series))
		for j, r := range t.Series {
			if len(r.Metric) > len(localMarker) && r.Metric[:len(localMarker)] == localMarker {
				r.Metric = "plugin." + pluginID + "." + r.Metric[len(localMarker):]
			}
			refs[j] = r
		}
		nt := *t
		nt.Series = refs
		ws[i].Timeseries = &nt
	}
	return ws
}
