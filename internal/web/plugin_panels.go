package web

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type panelStore struct{ path string }

func newPanelStore(stateDir string) panelStore {
	return panelStore{path: filepath.Join(stateDir, "plugin_panels.json")}
}

func (s panelStore) load() map[string][]string {
	out := map[string][]string{}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (s panelStore) Get(user string) []string {
	if s.path == "" || user == "" {
		return nil
	}
	defer lockStore(s.path)()
	return s.load()[user]
}

func (s panelStore) Set(user string, panels []string) error {
	defer lockStore(s.path)()
	all := s.load()
	if len(panels) == 0 {
		delete(all, user)
	} else {
		all[user] = panels
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".plugin_panels-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

type panelChoice struct {
	Key, Plugin, Title string
	Checked            bool
}

type PanelsPageData struct {
	PageData
	Choices []panelChoice
}

func panelKey(o plugin.SlotOffer) string { return o.PluginID + "/" + o.SlotID }

// chosenPanels returns the user's chosen dashboard panels that are still offered, in stored order.
func chosenPanels(r *http.Request, d Deps) []pluginSlotRef {
	offers := slotOffers(r, d, plugin.SlotDashboardPanel)
	if len(offers) == 0 {
		return nil
	}
	byKey := map[string]plugin.SlotOffer{}
	for _, o := range offers {
		byKey[panelKey(o)] = o
	}
	var out []pluginSlotRef
	for _, k := range newPanelStore(d.StateDir).Get(auditUser(r)) {
		if o, ok := byKey[k]; ok {
			out = append(out, slotRefFor(r, o, ""))
		}
	}
	return out
}

func panelsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := PanelsPageData{PageData: newPageData(r, d, "Dashboard panels", "Choose which plugin panels appear on your dashboard")}
		chosen := map[string]bool{}
		for _, k := range newPanelStore(d.StateDir).Get(auditUser(r)) {
			chosen[k] = true
		}
		for _, o := range slotOffers(r, d, plugin.SlotDashboardPanel) {
			data.Choices = append(data.Choices, panelChoice{Key: panelKey(o), Plugin: o.PluginName, Title: o.Title, Checked: chosen[panelKey(o)]})
		}
		if err := renderIntegrationsPageStatus(w, "plugin_panels.html", data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func panelsSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		valid := map[string]bool{}
		for _, o := range slotOffers(r, d, plugin.SlotDashboardPanel) {
			valid[panelKey(o)] = true
		}
		var picked []string
		seen := map[string]bool{}
		for _, k := range r.PostForm["panel"] {
			if valid[k] && !seen[k] {
				seen[k] = true
				picked = append(picked, k)
			}
		}
		if err := newPanelStore(d.StateDir).Set(auditUser(r), picked); err != nil {
			http.Error(w, "could not save panels", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}
