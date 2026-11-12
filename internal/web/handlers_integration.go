package web

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type permLine struct {
	Name string
	Desc string
}

func permLines(ps []plugin.Permission) []permLine {
	out := make([]permLine, len(ps))
	for i, p := range ps {
		out[i] = permLine{Name: string(p), Desc: plugin.Describe(p)}
	}
	return out
}

type integrationPageOpts struct {
	Overlay  map[string]string
	Errs     plugin.FieldErrors
	Section  plugin.Slot
	Flash    string
	FlashErr bool
}

type IntegrationPageData struct {
	PageData
	Plugin       integrationRow
	Perms        []permLine
	Consents     []plugin.ConsentRecord
	Logs         []plugin.LogEntry
	Files        []plugin.FileDecl
	Settings     []configField
	AlertRules   []configField
	Flash        string
	FlashErr     bool
	NeedsConsent bool
	HasPending   bool
	Push         *pushView
}

func renderPluginErr(w http.ResponseWriter, r *http.Request, d Deps, err error) {
	if errors.Is(err, plugin.ErrNotFound) {
		renderNotFound(w, r, d, "no such plugin")
		return
	}
	log.Printf("web: plugin: %v", err)
	http.Error(w, "plugin manager error", http.StatusInternalServerError)
}

func resolveIntegrationFlash(r *http.Request) (string, bool) {
	switch r.URL.Query().Get("flash") {
	case "saved":
		return "Settings saved.", false
	case "enabled":
		return "Plugin enabled.", false
	case "disabled":
		return "Plugin disabled.", false
	case "enable-error":
		return "The plugin could not be enabled. See the daemon log for details.", true
	case "pushed":
		return "Push started.", false
	case "push-error":
		return "The push failed. See the daemon log for details.", true
	}
	return "", false
}

func renderIntegrationDetail(w http.ResponseWriter, r *http.Request, d Deps, id string, opts integrationPageOpts, status int) {
	m := pluginManager(d)
	if m == nil {
		renderNotFound(w, r, d, "plugins are unavailable")
		return
	}
	st, err := m.Get(id)
	if err != nil {
		renderPluginErr(w, r, d, err)
		return
	}
	rc, err := m.Config(id)
	if err != nil {
		log.Printf("web: plugin %s config: %v", id, err)
	}
	data := IntegrationPageData{
		PageData: newPageData(r, d, st.Manifest.Name, "Integration details"),
		Plugin:   newIntegrationRow(st),
		Perms:    permLines(st.Manifest.Permissions),
		Consents: st.Consents,
		Logs:     st.Health.RecentLogs,
		Files:    st.Manifest.Contributes.Files,
	}
	data.Title = orDash(st.Manifest.Name)
	if st.ID != "" && st.Manifest.Name == "" {
		data.Title = st.ID
	}
	data.Flash, data.FlashErr = resolveIntegrationFlash(r)
	if opts.Flash != "" {
		data.Flash, data.FlashErr = opts.Flash, opts.FlashErr
	}
	var settingsOverlay, alertOverlay map[string]string
	var settingsErrs, alertErrs plugin.FieldErrors
	switch opts.Section {
	case plugin.SlotSettingsForm:
		settingsOverlay, settingsErrs = opts.Overlay, opts.Errs
	case plugin.SlotAlertRules:
		alertOverlay, alertErrs = opts.Overlay, opts.Errs
	}
	data.Settings = buildConfigFields(rc, plugin.SlotSettingsForm, settingsOverlay, settingsErrs)
	data.AlertRules = buildConfigFields(rc, plugin.SlotAlertRules, alertOverlay, alertErrs)
	data.NeedsConsent = st.State != plugin.StateEnabled && st.State != plugin.StateRevoked && st.State != plugin.StateIncompatible
	data.HasPending = st.Pending != nil
	data.Push = buildPushView(r, d, m, st)
	if err := renderIntegrationsPageStatus(w, "integration.html", data, status); err != nil {
		log.Printf("web: render integration: %v", err)
	}
}

func integrationDetailHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		renderIntegrationDetail(w, r, d, r.PathValue("id"), integrationPageOpts{}, http.StatusOK)
	}
}

type ConsentPageData struct {
	PageData
	Plugin     integrationRow
	Perms      []permLine
	Unverified bool
	Disallowed bool
	Pending    bool
	Warning    []string
}

func isUnverifiedTrust(t plugin.Trust) bool {
	return t != plugin.TrustVerified && t != plugin.TrustFirstParty
}

// consentTarget returns the permissions the user must accept, the trust being consented to and the manifest shown.
func consentTarget(st plugin.Status, pending bool) (perms []plugin.Permission, trust plugin.Trust, mf plugin.Manifest, ok bool) {
	if pending {
		if st.Pending == nil {
			return nil, "", plugin.Manifest{}, false
		}
		mf = st.Pending.Manifest
		if mf.Name == "" {
			mf.Name = st.Manifest.Name
		}
		if mf.Version == "" {
			mf.Version = st.Pending.Version
		}
		if mf.Publisher == "" {
			mf.Publisher = st.Manifest.Publisher
		}
		return st.Pending.NewPermissions, st.Pending.Trust, mf, true
	}
	return st.Manifest.Permissions, st.Trust, st.Manifest, true
}

func integrationEnablePageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		st, err := m.Get(r.PathValue("id"))
		if err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		pending := r.URL.Query().Get("pending") == "1"
		perms, trust, mf, ok := consentTarget(st, pending)
		if !ok {
			renderNotFound(w, r, d, "no pending update")
			return
		}
		data := ConsentPageData{
			PageData: newPageData(r, d, "Enable "+orDash(mf.Name), "Review what this plugin may do"),
			Plugin:   newIntegrationRow(st),
			Perms:    permLines(perms),
			Pending:  pending,
		}
		if isUnverifiedTrust(trust) {
			data.Unverified = true
			data.Disallowed = !d.Cfg().PluginsAllowUnverified()
			data.Warning = strings.Split(plugin.RiskWarning(mf.Name, mf.Version, mf.Publisher), "\n")
		}
		if err := renderIntegrationsPageStatus(w, "plugin_consent.html", data, http.StatusOK); err != nil {
			log.Printf("web: render consent: %v", err)
		}
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func integrationEnableHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
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
		pending := r.PostForm.Get("pending") == "1"
		perms, trust, _, ok := consentTarget(st, pending)
		if !ok {
			renderNotFound(w, r, d, "no pending update")
			return
		}
		expected := make([]string, len(perms))
		for i, p := range perms {
			expected[i] = string(p)
		}
		ack := false
		if isUnverifiedTrust(trust) {
			if !d.Cfg().PluginsAllowUnverified() {
				http.Error(w, "unverified plugins are disabled", http.StatusForbidden)
				return
			}
			if !sameSet(r.PostForm["accept"], expected) {
				http.Error(w, "every permission must be accepted", http.StatusBadRequest)
				return
			}
			if r.PostForm.Get("ack_risk") == "" {
				http.Error(w, "the risk acknowledgement is required", http.StatusBadRequest)
				return
			}
			ack = true
		}
		err = m.Enable(plugin.EnableRequest{ID: id, Actor: auditUser(r), Via: "web", Accepted: perms, AcknowledgedRisk: ack, Pending: pending})
		if err != nil {
			log.Printf("web: plugin enable %q: %v", id, err)
			if errors.Is(err, plugin.ErrUnverifiedDisallowed) {
				http.Error(w, "unverified plugins are disabled", http.StatusForbidden)
				return
			}
			redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "enable-error")
			return
		}
		logAudit(d, r, "plugin.consent", id, "", strings.Join(expected, ","))
		logAudit(d, r, "plugin.enable", id, "", st.Manifest.Version)
		redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "enabled")
	}
}

func integrationDisableHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		if err := m.Disable(id, auditUser(r), "web"); err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		logAudit(d, r, "plugin.disable", id, "", "")
		redirectIntegrations(w, r, "/integrations/"+url.PathEscape(id), "disabled")
	}
}

func integrationRemoveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		m := pluginManager(d)
		if m == nil {
			renderNotFound(w, r, d, "plugins are unavailable")
			return
		}
		if err := m.Remove(id, auditUser(r), "web"); err != nil {
			renderPluginErr(w, r, d, err)
			return
		}
		logAudit(d, r, "plugin.remove", id, "", "")
		http.Redirect(w, r, "/integrations", http.StatusSeeOther)
	}
}
