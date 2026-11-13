package web

import (
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type pushView struct {
	Nodes      []core.NodeSummary
	Tags       []string
	Perms      []permLine
	Unverified bool
	Warning    []string
	States     []plugin.NodeState
}

func buildPushView(r *http.Request, d Deps, m plugin.Manager, st plugin.Status) *pushView {
	isMaster, nodes := resolveMasterAndNodes(r, d)
	if !isMaster {
		return nil
	}
	pv := &pushView{Perms: permLines(st.Manifest.Permissions)}
	tags := map[string]bool{}
	for _, n := range nodes {
		if n.Self {
			continue
		}
		pv.Nodes = append(pv.Nodes, n)
		for _, t := range n.Tags {
			tags[t] = true
		}
	}
	for t := range tags {
		pv.Tags = append(pv.Tags, t)
	}
	sort.Strings(pv.Tags)
	if isUnverifiedTrust(st.Trust) {
		pv.Unverified = true
		pv.Warning = strings.Split(plugin.RiskWarning(st.Manifest.Name, st.Manifest.Version, st.Manifest.Publisher), "\n")
	}
	if states, err := m.NodeStates(st.ID); err == nil {
		pv.States = states
	}
	return pv
}

func integrationPushHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		if isMaster, _ := resolveMasterAndNodes(r, d); !isMaster {
			renderNotFound(w, r, d, "not a fleet master")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		st, err := m.Get(id)
		if err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		nodes, tag := r.PostForm["node"], strings.TrimSpace(r.PostForm.Get("tag"))
		remove := r.PostForm.Get("remove") == "1"
		if len(nodes) == 0 && tag == "" {
			http.Error(w, "choose at least one node or a tag", http.StatusBadRequest)
			return
		}
		req := plugin.PushRequest{ID: id, Nodes: nodes, Tag: tag, Actor: auditUser(r), Via: "web", Remove: remove}
		if !remove && isUnverifiedTrust(st.Trust) {
			if !d.Cfg().PluginsAllowUnverified() {
				http.Error(w, "unverified plugins are disabled", http.StatusForbidden)
				return
			}
			expected := make([]string, len(st.Manifest.Permissions))
			for i, p := range st.Manifest.Permissions {
				expected[i] = string(p)
			}
			if !sameSet(r.PostForm["accept"], expected) || r.PostForm.Get("ack_risk") == "" {
				http.Error(w, "every permission and the risk acknowledgement are required", http.StatusBadRequest)
				return
			}
			req.AcknowledgedRisk, req.Accepted = true, st.Manifest.Permissions
		}
		if _, err := m.Push(req); err != nil {
			log.Printf("web: plugin push %q: %v", id, err)
			redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "push-error")
			return
		}
		target := strings.Join(nodes, ",")
		if tag != "" {
			target += " tag:" + tag
		}
		logAudit(d, r, "plugin.push", id, "", strings.TrimSpace(target))
		redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "pushed")
	}
}
