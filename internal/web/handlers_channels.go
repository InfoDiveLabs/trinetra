//go:build web

package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"serverwatch/internal/config"
)

// channelsMutation composes requireRole(RoleAdmin, ...) with requireCSRF,
// mirroring usersMutation/configMutation: every /channels* mutation (add,
// update, remove, test) needs both gates.
func channelsMutation(next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// channelNameParam/channelNameFromParam convert a channel's Name (arbitrary
// text — "Ops email", "#infra" — see config.ChannelConfig.Name) to/from the
// URL-safe form the {name} path segment carries, reusing the exact encoding
// handlers_users.go's credentialParam/credentialFromParam already use for
// the same reason (a raw name isn't always a safe single path segment).
func channelNameParam(name string) string { return credentialParam([]byte(name)) }
func channelNameFromParam(param string) (string, error) {
	b, err := credentialFromParam(param)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// channelRow is one row of the /channels table.
type channelRow struct {
	Name        string
	Type        string
	Enabled     bool
	MinSeverity string
	Routes      string
	Param       string // channelNameParam(Name), for this row's action URLs
}

// channelTypes is the fixed set of channel types the add/edit modal offers
// (mirrors buildNotifier's switch, channel.go), in the mockup's display
// order.
var channelTypes = []struct{ Value, Label string }{
	{"telegram", "Telegram"},
	{"email", "Email (SMTP)"},
	{"ntfy", "ntfy / Gotify"},
	{"slack", "Slack / Discord"},
	{"webhook", "Generic webhook"},
}

// describeRoutesWeb mirrors channel.go's describeRoutes (unexported to that
// package, so duplicated here rather than reached into across the
// serverwatch/web boundary — see the design note atop
// internal/serverwatch/web_deps.go for why internal/web can't import
// serverwatch to share it directly).
func describeRoutesWeb(cc config.ChannelConfig) string {
	var parts []string
	if len(cc.IncludeKinds) > 0 {
		parts = append(parts, "include="+strings.Join(cc.IncludeKinds, ","))
	}
	if len(cc.ExcludeKinds) > 0 {
		parts = append(parts, "exclude="+strings.Join(cc.ExcludeKinds, ","))
	}
	if cc.CriticalOverridesQuiet {
		parts = append(parts, "critical-overrides-quiet")
	}
	if len(parts) == 0 {
		return "all severities"
	}
	return strings.Join(parts, " ")
}

// channelRows builds the /channels table rows from cfg.Channels, sorted by
// name for a stable render order (Channels is a plain slice in append
// order, which would otherwise reorder every time a channel is added).
func channelRows(cfg *config.Config) []channelRow {
	rows := make([]channelRow, 0, len(cfg.Channels))
	for _, cc := range cfg.Channels {
		sev := cc.MinSeverity
		if sev == "" {
			sev = "info"
		}
		rows = append(rows, channelRow{
			Name:        cc.Name,
			Type:        cc.Type,
			Enabled:     cc.Enabled,
			MinSeverity: sev,
			Routes:      describeRoutesWeb(cc),
			Param:       channelNameParam(cc.Name),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// channelModalData is one add/edit channel modal's render data: the same
// shape backs both the single "Add channel" modal (IsEdit false, all fields
// at their zero value/default) and one per-existing-channel "Edit channel"
// modal (IsEdit true, pre-filled from that channel's current config) — see
// templates/channels.html's "chanModalBody" block, defined once and
// executed for each. Rendering N small edit modals server-side (rather than
// one shared modal the mockup's app.js JS-prefills on open) avoids needing
// to thread secret-bearing Settings values through JS data-* attributes and
// avoids the CSP's no-inline-script constraint entirely: every field is
// simply already correct by the time the page loads.
type channelModalData struct {
	ID                     string // DOM id: "chanModal-new" or "chanModal-<param>"
	Title                  string
	Action                 string // form action: "/channels" or "/channels/<param>/update"
	IsEdit                 bool
	Param                  string // channelNameParam(Name), edit only — for the Send-test button's formaction
	Name                   string
	Type                   string
	Enabled                bool
	MinSeverity            string
	IncludeKinds           string
	ExcludeKinds           string
	CriticalOverridesQuiet bool
	Settings               map[string]string
	// CSRF is threaded in directly (rather than read via the outer page's
	// "$" inside the shared "chanModalBody" template) because {{template
	// "name" pipeline}} gives that block a FRESH "$" scoped to pipeline
	// itself, not inherited from the content block that invoked it.
	CSRF string
}

// newChannelModalData is the blank "Add channel" modal's data: telegram
// (the mockup's default selected type) with an empty Settings map so
// `{{index .Settings "..."}}` always resolves to "" rather than needing a
// nil-map guard in the template.
var newChannelModalData = channelModalData{
	ID:          "chanModal-new",
	Title:       "Add channel",
	Action:      "/channels",
	Type:        "telegram",
	MinSeverity: "info",
	Settings:    map[string]string{},
}

// channelModalFor builds cc's edit-modal data.
func channelModalFor(cc config.ChannelConfig) channelModalData {
	sev := cc.MinSeverity
	if sev == "" {
		sev = "info"
	}
	settings := cc.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	param := channelNameParam(cc.Name)
	return channelModalData{
		ID:                     "chanModal-" + param,
		Title:                  "Edit channel",
		Action:                 "/channels/" + param + "/update",
		IsEdit:                 true,
		Param:                  param,
		Name:                   cc.Name,
		Type:                   cc.Type,
		Enabled:                cc.Enabled,
		MinSeverity:            sev,
		IncludeKinds:           strings.Join(cc.IncludeKinds, ","),
		ExcludeKinds:           strings.Join(cc.ExcludeKinds, ","),
		CriticalOverridesQuiet: cc.CriticalOverridesQuiet,
		Settings:               settings,
	}
}

// ChannelsPageData is what templates/channels.html renders against.
type ChannelsPageData struct {
	PageData
	Channels     []channelRow
	ChannelTypes []struct{ Value, Label string }
	NewModal     channelModalData
	EditModals   []channelModalData
	// TestResult, if non-empty, is rendered as a one-line status after a
	// "Send test" action — success or the error message — since a test-send
	// has nothing to persist and nothing else to show for it.
	TestResult string
}

func buildChannelsPageData(r *http.Request, d Deps, testResult string) ChannelsPageData {
	cfg := d.Cfg()
	page := newPageData(r, "Notification channels", "Where alerts are delivered", "ok")

	newModal := newChannelModalData
	newModal.CSRF = page.CSRF
	modals := make([]channelModalData, 0, len(cfg.Channels))
	for _, cc := range cfg.Channels {
		m := channelModalFor(cc)
		m.CSRF = page.CSRF
		modals = append(modals, m)
	}
	sort.Slice(modals, func(i, j int) bool { return modals[i].Name < modals[j].Name })
	return ChannelsPageData{
		PageData:     page,
		Channels:     channelRows(cfg),
		ChannelTypes: channelTypes,
		NewModal:     newModal,
		EditModals:   modals,
		TestResult:   testResult,
	}
}

func renderChannelsPage(w http.ResponseWriter, data ChannelsPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/channels.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// channelsPageHandler renders GET /channels. requireRole(RoleAdmin, ...)
// (routes.go's wiring) has already gated this by the time it runs.
func channelsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildChannelsPageData(r, d, "")
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// channelSettingKeys lists, per channel type, which posted "settings.<k>"
// form fields channelSettingsFromForm reads — the fields
// templates/channels.html's per-type <div data-cond="..."> blocks actually
// render (see that template), so a channel's Settings map only ever picks
// up keys relevant to its own type.
var channelSettingKeys = map[string][]string{
	"telegram": {"token", "chat_id"},
	"email":    {"host", "port", "username", "password", "from", "to"},
	"ntfy":     {"server", "topic"},
	"slack":    {"url"},
	"webhook":  {"url", "method", "template"},
}

// channelSettingsFromForm reads settings.<k> fields for typ from r (already
// ParseForm'd), skipping any that were left blank — an admin editing one
// field (e.g. rotating a token) shouldn't be forced to retype every other
// setting, and config.SetChannelField only ever overwrites a key it's
// explicitly given.
func channelSettingsFromForm(r *http.Request, typ string) map[string]string {
	out := map[string]string{}
	for _, k := range channelSettingKeys[typ] {
		if v := strings.TrimSpace(r.FormValue("settings." + k)); v != "" {
			out[k] = v
		}
	}
	return out
}

// applyChannelForm applies every posted channel field onto the named
// channel via config.Config.SetChannelField (reusing its existing
// validators, e.g. min_severity) — used by both channelsAddHandler (after
// AddChannel creates the row) and channelsUpdateHandler (channel already
// exists). Returns the first validation error, if any; the caller is
// responsible for not persisting when that happens.
func applyChannelForm(newCfg *config.Config, name string, r *http.Request) error {
	typ := r.FormValue("type")
	if err := newCfg.SetChannelField(name, "type", typ); err != nil {
		return err
	}
	if err := newCfg.SetChannelField(name, "enabled", boolFormValue(r, "enabled")); err != nil {
		return err
	}
	if err := newCfg.SetChannelField(name, "min_severity", r.FormValue("min_severity")); err != nil {
		return err
	}
	if err := newCfg.SetChannelField(name, "critical_overrides_quiet", boolFormValue(r, "critical_overrides_quiet")); err != nil {
		return err
	}
	if v := r.FormValue("include_kinds"); v != "" {
		if err := newCfg.SetChannelField(name, "include_kinds", v); err != nil {
			return err
		}
	}
	if v := r.FormValue("exclude_kinds"); v != "" {
		if err := newCfg.SetChannelField(name, "exclude_kinds", v); err != nil {
			return err
		}
	}
	for k, v := range channelSettingsFromForm(r, typ) {
		if err := newCfg.SetChannelField(name, "setting."+k, v); err != nil {
			return err
		}
	}
	return nil
}

// boolFormValue renders a posted boolean-ish field as "true"/"false" for
// config.SetChannelField's strconv.ParseBool-based enabled/
// critical_overrides_quiet keys. Two calling conventions both work: a real
// checkbox (present with some truthy value like "1" when checked, entirely
// ABSENT — not merely empty — when unchecked, per HTML form semantics), or
// a hidden field carrying an explicit "true"/"false" literal (the channels
// table's per-row enabled-toggle button, which needs to post the OPPOSITE
// of the row's current state rather than "checked/unchecked").
func boolFormValue(r *http.Request, field string) string {
	v := r.FormValue(field)
	if v == "" {
		return "false"
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return strconv.FormatBool(b)
	}
	// Present with some other truthy placeholder (e.g. a checkbox's
	// value="1") -> checked.
	return "true"
}

// channelsAddHandler handles POST /channels: creates a new channel from the
// posted name/type/settings/routing fields. Validates against a clone of
// the current config (cloneConfig, handlers_config.go) before persisting,
// same "bad value -> 400, no write" contract as /config.
func channelsAddHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		if name == "" {
			http.Error(w, "channel name is required", http.StatusBadRequest)
			return
		}
		newCfg, err := cloneConfig(d.Cfg())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if _, ok := newCfg.GetChannel(name); ok {
			http.Error(w, fmt.Sprintf("channel %q already exists", name), http.StatusConflict)
			return
		}
		newCfg.AddChannel(config.ChannelConfig{Name: name})
		if err := applyChannelForm(newCfg, name, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Reload(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "channel.add", name, "", channelSummary(newCfg, name))

		data := buildChannelsPageData(r, d, "")
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// channelSummary renders name's current config as a short human string for
// the audit log's New/Old columns.
func channelSummary(cfg *config.Config, name string) string {
	cc, ok := cfg.GetChannel(name)
	if !ok {
		return ""
	}
	return fmt.Sprintf("type=%s enabled=%v min_severity=%s", cc.Type, cc.Enabled, cc.MinSeverity)
}

// channelsUpdateHandler handles POST /channels/{name}/update: applies the
// posted fields onto the EXISTING named channel, same validation/no-write
// contract as channelsAddHandler.
func channelsUpdateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := channelNameFromParam(r.PathValue("name"))
		if err != nil {
			http.Error(w, "invalid channel", http.StatusBadRequest)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		oldCfg := d.Cfg()
		if _, ok := oldCfg.GetChannel(name); !ok {
			http.Error(w, "unknown channel", http.StatusNotFound)
			return
		}
		oldSummary := channelSummary(oldCfg, name)

		newCfg, err := cloneConfig(oldCfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := applyChannelForm(newCfg, name, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Reload(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "channel.update", name, oldSummary, channelSummary(newCfg, name))

		data := buildChannelsPageData(r, d, "")
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// channelsRemoveHandler handles POST /channels/{name}/remove.
func channelsRemoveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := channelNameFromParam(r.PathValue("name"))
		if err != nil {
			http.Error(w, "invalid channel", http.StatusBadRequest)
			return
		}
		oldCfg := d.Cfg()
		oldSummary := channelSummary(oldCfg, name)
		newCfg, err := cloneConfig(oldCfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !newCfg.RemoveChannel(name) {
			http.Error(w, "unknown channel", http.StatusNotFound)
			return
		}
		if err := d.Reload(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "channel.remove", name, oldSummary, "")

		data := buildChannelsPageData(r, d, "")
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// channelsTestHandler handles POST /channels/{name}/test: sends a one-off
// test notification via Deps.TestChannel (wired to
// internal/serverwatch/daemon.go's testChannel closure in a real `-tags
// web` binary — see server.go's Deps.TestChannel doc). A nil TestChannel
// (some minimal test Deps, or a hypothetical future non-serverwatch host of
// this package) renders a clear "not wired" result rather than panicking.
func channelsTestHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := channelNameFromParam(r.PathValue("name"))
		if err != nil {
			http.Error(w, "invalid channel", http.StatusBadRequest)
			return
		}
		result := fmt.Sprintf("sent test notification via %q", name)
		if d.TestChannel == nil {
			result = "channel testing is not wired up in this build"
		} else if err := d.TestChannel(name); err != nil {
			result = fmt.Sprintf("test failed: %v", err)
		}

		data := buildChannelsPageData(r, d, result)
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
