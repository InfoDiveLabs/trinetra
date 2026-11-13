package web

import (
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type pluginSlotRef struct {
	Plugin, SlotID, Title, Src string
}

// pluginNodeID is the fleet node a request is scoped to, or "" for this host.
func pluginNodeID(r *http.Request) string {
	if ns := nodeFrom(r); !ns.Self {
		return ns.ID
	}
	return ""
}

// slotOffers lists the slot offers of one kind the signed-in user's role may see; it never calls a plugin.
func slotOffers(r *http.Request, d Deps, slot plugin.Slot) []plugin.SlotOffer {
	m := pluginManager(d)
	if m == nil {
		return nil
	}
	offers, err := m.Slots(pluginNodeID(r), slot)
	if err != nil {
		log.Printf("web: plugin slots %s: %v", slot, err)
		return nil
	}
	rank := plugin.RoleRank(plugin.Role(currentRole(r)))
	var out []plugin.SlotOffer
	for _, o := range offers {
		if rank >= plugin.RoleRank(o.MinRole) && rank > 0 {
			out = append(out, o)
		}
	}
	return out
}

func slotRefFor(r *http.Request, o plugin.SlotOffer, container string) pluginSlotRef {
	q := url.Values{"slot": {string(o.Slot)}, "plugin": {o.PluginID}, "slot_id": {o.SlotID}}
	if container != "" {
		q.Set("container", container)
	}
	return pluginSlotRef{Plugin: o.PluginName, SlotID: o.SlotID, Title: o.Title, Src: nodeHref(nodeFrom(r).Prefix, "/plugins/slot?"+q.Encode())}
}

func slotRefs(r *http.Request, d Deps, slot plugin.Slot) []pluginSlotRef {
	var out []pluginSlotRef
	for _, o := range slotOffers(r, d, slot) {
		out = append(out, slotRefFor(r, o, ""))
	}
	return out
}

func pluginNavFor(r *http.Request, d Deps) []NavItem {
	var out []NavItem
	prefix := nodeFrom(r).Prefix
	for _, o := range slotOffers(r, d, plugin.SlotNavPage) {
		out = append(out, NavItem{Href: nodeHref(prefix, "/p/"+url.PathEscape(o.PluginID)+"/"+url.PathEscape(o.SlotID)), Icon: "plug", Label: o.Title})
	}
	return out
}

type PluginPageData struct {
	PageData
	Slot pluginSlotRef
}

func pluginPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid, sid := r.PathValue("plugin"), r.PathValue("page")
		for _, o := range slotOffers(r, d, plugin.SlotNavPage) {
			if o.PluginID != pid || o.SlotID != sid {
				continue
			}
			data := PluginPageData{PageData: newPageData(r, d, o.Title, o.PluginName), Slot: slotRefFor(r, o, "")}
			if err := renderIntegrationsPageStatus(w, "plugin_page.html", data, http.StatusOK); err != nil {
				log.Printf("web: render plugin page: %v", err)
			}
			return
		}
		renderNotFound(w, r, d, "no such page")
	}
}

func pluginSlotHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		slot := plugin.Slot(q.Get("slot"))
		known := false
		for _, s := range plugin.V1Slots {
			known = known || s == slot
		}
		if !known || slot.HostRendered() {
			http.Error(w, "unknown slot", http.StatusBadRequest)
			return
		}
		container := q.Get("container")
		offers := slotOffers(r, d, slot)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		pid, sid := q.Get("plugin"), q.Get("slot_id")
		if pid == "" {
			for _, o := range offers {
				ref := slotRefFor(r, o, container)
				if slot == plugin.SlotContainerSection && o.Title != "" {
					fmt.Fprintf(w, `<div class="section-label plugin-section-label"><span class="eyebrow">%s</span></div>`, html.EscapeString(o.Title))
				}
				fmt.Fprintf(w, `<div class="plugin-slot-host" hx-get="%s" hx-trigger="load" hx-swap="innerHTML"><div class="note">Loading %s&hellip;</div></div>`,
					html.EscapeString(ref.Src), html.EscapeString(o.PluginName))
			}
			return
		}
		for _, o := range offers {
			if o.PluginID != pid || o.SlotID != sid {
				continue
			}
			req := plugin.UIRequest{PluginID: pid, Node: pluginNodeID(r), Request: plugin.SlotRequest{
				Slot: slot, SlotID: sid, Context: plugin.SlotContext{Node: pluginNodeID(r), Container: container},
			}}
			_, _ = w.Write([]byte(renderSlot(r.Context(), d, r, req)))
			return
		}
		http.NotFound(w, r)
	}
}

type PluginActionPageData struct {
	PageData
	Back    string
	Label   string
	OK      bool
	Message string
	Widgets template.HTML
}

// actionBackPath is the local path the action was submitted from, or "/" when the Referer is missing or not this site.
func actionBackPath(r *http.Request) string {
	u, err := url.Parse(r.Header.Get("Referer"))
	if err != nil || u.Host != r.Host || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || u.Path == "/plugins/action" {
		return "/"
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

func pluginActionHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		pid, aid := r.PostForm.Get("plugin"), r.PostForm.Get("action")
		st, err := m.Get(pid)
		if err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		decl, ok := st.Manifest.ActionDecl(aid)
		if !ok {
			renderNotFound(w, r, d, "no such action")
			return
		}
		if plugin.RoleRank(plugin.Role(currentRole(r))) < plugin.RoleRank(decl.MinRole) {
			renderDenied(w, r, d)
			return
		}
		confirmed := r.PostForm.Get("confirmed") == "1"
		if decl.Mutating && !confirmed {
			http.Error(w, "this action needs confirmation", http.StatusBadRequest)
			return
		}
		params := map[string]string{}
		for k, v := range r.PostForm {
			if name, ok := strings.CutPrefix(k, "param."); ok && len(v) > 0 && len(params) < 64 && len(v[0]) <= plugin.MaxWidgetString {
				params[name] = v[0]
			}
		}
		node, container := r.PostForm.Get("node"), r.PostForm.Get("container")
		user := plugin.UserContext{Name: auditUser(r), Role: plugin.Role(currentRole(r))}
		res, err := m.InvokeAction(plugin.UIActionRequest{PluginID: pid, Node: node, Request: plugin.ActionRequest{
			Action: aid, Context: plugin.SlotContext{Node: node, Container: container}, User: user, Params: params, Confirmed: confirmed,
		}})
		if decl.Mutating {
			logAudit(d, r, "plugin.action", pid+"/"+aid, "", "")
		}
		data := PluginActionPageData{PageData: newPageData(r, d, decl.Label, st.Manifest.Name), Label: decl.Label, Back: actionBackPath(r)}
		if err != nil {
			log.Printf("web: plugin %s action %s: %v", pid, aid, err)
			data.Message = "The action failed. Details are in the plugin's log."
		} else {
			data.OK, data.Message = res.OK, res.Message
			if len(res.Widgets) > 0 && validateRender(plugin.RenderResult{Widgets: res.Widgets}, st) == nil {
				csrf := ""
				if sess, ok := sessionFromContext(r); ok {
					csrf = sess.CSRF
				}
				data.Widgets = renderWidgets(st, res.Widgets, widgetCtx{CSRF: csrf, Node: node, Container: container, IDPrefix: "pa-" + pid + "-" + aid})
			}
		}
		if err := renderIntegrationsPageStatus(w, "plugin_action.html", data, http.StatusOK); err != nil {
			log.Printf("web: render plugin action: %v", err)
		}
	}
}

func pluginFileHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := pluginManager(d)
		name := r.PathValue("name")
		if m == nil || !plugin.FileNamePattern.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		decl, data, err := m.File(r.PathValue("id"), name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		h.Set("Content-Type", decl.ContentType)
		h.Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
	}
}
