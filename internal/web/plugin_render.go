package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const slotWebDeadline = plugin.RenderSlotTimeout + 500*time.Millisecond

type kvPair struct{ K, V string }

type ActionView struct {
	PluginID  string
	ID        string
	Label     string
	Confirm   string
	Mutating  bool
	Params    []kvPair
	Node      string
	Container string
	CSRF      string
}

type fieldView struct {
	Name, Label, Type, Value string
	Options                  []string
	Required, Checked        bool
}

type formView struct {
	ActionView
	Submit string
	Fields []fieldView
}

type statusItemView struct {
	Name, Detail, Led string
}

type widgetView struct {
	Kind      string
	Title     string
	ChartID   string
	Metrics   string
	Labels    string
	Range     string
	Unit      string
	Stat      *plugin.StatWidget
	Table     *plugin.TableWidget
	KV        []plugin.KV
	LogText   string
	Form      *formView
	Action    *ActionView
	TextHTML  template.HTML
	TextPlain string
	Items     []statusItemView
}

type slotView struct {
	PluginID string
	Widgets  []widgetView
}

type errorCardView struct {
	Name  string
	Class string
}

var (
	widgetTmplOnce sync.Once
	widgetTmpl     *template.Template
	widgetTmplErr  error
)

func widgetTemplates() (*template.Template, error) {
	widgetTmplOnce.Do(func() {
		widgetTmpl, widgetTmplErr = template.New("plugin_widgets.html").Funcs(funcMap).ParseFS(templatesFS, "templates/plugin_widgets.html")
	})
	return widgetTmpl, widgetTmplErr
}

func execWidgetTemplate(name string, data any) template.HTML {
	t, err := widgetTemplates()
	if err != nil {
		log.Printf("web: plugin widget templates: %v", err)
		return ""
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("web: plugin widget template %s: %v", name, err)
		return ""
	}
	return template.HTML(buf.String())
}

func errorCard(name, class string) template.HTML {
	return execWidgetTemplate("pw-error", errorCardView{Name: name, Class: class})
}

func isNotRunning(err error) bool {
	return errors.Is(err, plugin.ErrNotRunning) || errors.Is(err, plugin.ErrNotFound) || errors.Is(err, plugin.ErrUnavailable)
}

// renderSlot fetches, validates and renders one slot; any failure yields the error card for that slot only.
func renderSlot(ctx context.Context, d Deps, r *http.Request, req plugin.UIRequest) template.HTML {
	name := req.PluginID
	var mgr plugin.Manager
	if d.Plugins != nil {
		mgr = d.Plugins()
	}
	if mgr == nil {
		return errorCard(name, "is not running")
	}
	st, err := mgr.Get(req.PluginID)
	if err != nil {
		log.Printf("web: plugin %s slot %s: %v", req.PluginID, req.Request.SlotID, err)
		return errorCard(name, "is not running")
	}
	if st.Manifest.Name != "" {
		name = st.Manifest.Name
	}
	if req.Request.User.Name == "" {
		if u, ok := userFromContext(r); ok {
			req.Request.User = plugin.UserContext{Name: u.Name, Role: plugin.Role(u.Role)}
		}
	}
	if req.Node != "" {
		req.Request.Context.Node = req.Node
	}

	ctx, cancel := context.WithTimeout(ctx, slotWebDeadline)
	defer cancel()
	type result struct {
		res plugin.RenderResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		res, err := mgr.RenderSlot(req)
		ch <- result{res, err}
	}()
	var out result
	select {
	case out = <-ch:
	case <-ctx.Done():
		log.Printf("web: plugin %s slot %s: web deadline exceeded", req.PluginID, req.Request.SlotID)
		return errorCard(name, "timed out")
	}
	if out.err != nil {
		log.Printf("web: plugin %s slot %s: %v", req.PluginID, req.Request.SlotID, out.err)
		switch {
		case errors.Is(out.err, plugin.ErrDeadline) || errors.Is(out.err, context.DeadlineExceeded):
			return errorCard(name, "timed out")
		case isNotRunning(out.err):
			return errorCard(name, "is not running")
		}
		return errorCard(name, "failed")
	}
	if err := validateRender(out.res, st); err != nil {
		log.Printf("web: plugin %s slot %s: invalid view: %v", req.PluginID, req.Request.SlotID, err)
		return errorCard(name, "returned an invalid view")
	}
	csrf := ""
	if sess, ok := sessionFromContext(r); ok {
		csrf = sess.CSRF
	}
	return renderWidgets(st, out.res.Widgets, widgetCtx{
		CSRF: csrf, Node: req.Node, Container: req.Request.Context.Container,
		IDPrefix: "pc-" + req.PluginID + "-" + req.Request.SlotID,
	})
}

type widgetCtx struct {
	CSRF, Node, Container, IDPrefix string
}

func renderWidgets(st plugin.Status, ws []plugin.Widget, c widgetCtx) template.HTML {
	sv := slotView{PluginID: st.ID}
	for i, w := range ws {
		sv.Widgets = append(sv.Widgets, buildWidgetView(st, w, c, i))
	}
	return execWidgetTemplate("pw-slot", sv)
}

func newActionView(st plugin.Status, id string, params map[string]string, c widgetCtx) ActionView {
	decl, _ := st.Manifest.ActionDecl(id)
	av := ActionView{PluginID: st.ID, ID: id, Label: decl.Label, Confirm: decl.Confirm, Mutating: decl.Mutating, Node: c.Node, Container: c.Container, CSRF: c.CSRF}
	if av.Label == "" {
		av.Label = id
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		av.Params = append(av.Params, kvPair{k, params[k]})
	}
	return av
}

func buildWidgetView(st plugin.Status, w plugin.Widget, c widgetCtx, idx int) widgetView {
	v := widgetView{Kind: string(w.Kind), Title: w.Title}
	switch w.Kind {
	case plugin.WidgetTimeseries:
		t := w.Timeseries
		metrics := make([]string, len(t.Series))
		labels := make([]string, len(t.Series))
		for i, s := range t.Series {
			metrics[i] = s.Metric
			labels[i] = s.Label
			if labels[i] == "" {
				labels[i] = s.Metric
			}
		}
		v.ChartID = fmt.Sprintf("%s-%d", c.IDPrefix, idx)
		v.Metrics, v.Labels, v.Unit = strings.Join(metrics, ","), strings.Join(labels, ","), t.Unit
		v.Range = t.Range
		if v.Range == "" {
			v.Range = "6h"
		}
	case plugin.WidgetStat:
		v.Stat = w.Stat
	case plugin.WidgetTable:
		v.Table = w.Table
	case plugin.WidgetKV:
		v.KV = w.KV.Items
	case plugin.WidgetLog:
		v.LogText = strings.Join(w.Log.Lines, "\n")
	case plugin.WidgetForm:
		f := w.Form
		fv := &formView{ActionView: newActionView(st, f.Action, nil, c), Submit: f.Submit}
		if fv.Submit == "" {
			fv.Submit = fv.Label
		}
		for _, fl := range f.Fields {
			fld := fieldView{Name: fl.Name, Label: fl.Label, Type: fl.Type, Value: fl.Value, Options: fl.Options, Required: fl.Required}
			if fl.Type == "secret" {
				fld.Value = ""
			}
			if fl.Type == "boolean" {
				fld.Checked = fl.Value == "true"
			}
			fv.Fields = append(fv.Fields, fld)
		}
		v.Form = fv
	case plugin.WidgetAction:
		av := newActionView(st, w.Action.Action, w.Action.Params, c)
		v.Action = &av
	case plugin.WidgetText:
		if w.Text.Format == "lite" {
			v.TextHTML = sanitizeText(w.Text.Text)
		} else {
			v.TextPlain = w.Text.Text
		}
	case plugin.WidgetStatus:
		for _, it := range w.Status.Items {
			led := ""
			switch it.State {
			case "up":
				led = "ok"
			case "down":
				led = "crit"
			}
			v.Items = append(v.Items, statusItemView{Name: it.Name, Detail: it.Detail, Led: led})
		}
	}
	return v
}
