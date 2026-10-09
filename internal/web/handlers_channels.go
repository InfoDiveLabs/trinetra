package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// channelsMutation composes requireRole(RoleAdmin, ...) with requireCSRF, mirroring
// usersMutation/configMutation: every /channels* mutation.
func channelsMutation(d Deps, next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, d, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// channelNameParam/channelNameFromParam convert a channel's Name.
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
	Filtered    bool   // include/exclude kinds limit which alerts it gets
}

type deliveryRow struct {
	Severity, Label string
	Channels        []channelRow
}

// quietHoursText turns "23-8" into "23:00 to 08:00".
func quietHoursText(v string) string {
	from, to, ok := strings.Cut(v, "-")
	a, errA := strconv.Atoi(from)
	b, errB := strconv.Atoi(to)
	if !ok || errA != nil || errB != nil {
		return v
	}
	return fmt.Sprintf("%02d:00 to %02d:00", a, b)
}

var severityRank = map[string]int{"info": 0, "warning": 1, "critical": 2}

// deliveryRows lists, per severity, the enabled channels that receive it.
func deliveryRows(rows []channelRow) []deliveryRow {
	out := []deliveryRow{
		{Severity: "critical", Label: "Something is down or about to fail"},
		{Severity: "warning", Label: "Needs attention soon"},
		{Severity: "info", Label: "Recoveries, reports and notices"},
	}
	for i := range out {
		for _, c := range rows {
			if c.Enabled && severityRank[c.MinSeverity] <= severityRank[out[i].Severity] {
				out[i].Channels = append(out[i].Channels, c)
			}
		}
	}
	return out
}

// channelTypes is the fixed set of channel types the add/edit modal offers
// (mirrors buildNotifier's switch, channel.go), in display order.
var channelTypes = []struct{ Value, Label string }{
	{"telegram", "Telegram"},
	{"email", "Email (SMTP)"},
	{"ntfy", "ntfy / Gotify"},
	{"slack", "Slack / Discord"},
	{"webhook", "Generic webhook"},
}

// describeRoutesWeb mirrors channel.go's describeRoutes.
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

// channelRows builds the /channels table rows from cfg.Channels, sorted by name for a
// stable render order.
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
			Filtered:    len(cc.IncludeKinds) > 0 || len(cc.ExcludeKinds) > 0,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows
}

// channelModalData is one add/edit channel modal's render data: the same shape backs both
// the single "Add channel" modal.
type channelModalData struct {
	ID                     string // DOM id: "chanModal-new" or "chanModal-<param>"
	Title                  string
	Action                 string // form action: "/channels" or "/channels/<param>/update"
	IsEdit                 bool
	Param                  string // channelNameParam(Name), edit only -- for the Send-test button's formaction
	Name                   string
	Type                   string
	Enabled                bool
	MinSeverity            string
	IncludeKinds           string
	ExcludeKinds           string
	CriticalOverridesQuiet bool
	// SettingsMasked/SettingsSet are the template-safe view of the channel's Settings map
	// (maskChannelSettings): SettingsMasked never carries a secret value.
	SettingsMasked map[string]string
	SettingsSet    map[string]bool
	// CSRF is threaded in directly.
	CSRF string
}

// newChannelModalData is the blank "Add channel" modal's data: telegram.
var newChannelModalData = channelModalData{
	ID:             "chanModal-new",
	Title:          "Add channel",
	Action:         "/channels",
	Type:           "telegram",
	MinSeverity:    "info",
	SettingsMasked: map[string]string{},
	SettingsSet:    map[string]bool{},
}

// channelModalFor builds cc's edit-modal data.
func channelModalFor(cc config.ChannelConfig) channelModalData {
	sev := cc.MinSeverity
	if sev == "" {
		sev = "info"
	}
	masked, set := maskChannelSettings(cc.Type, cc.Settings)
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
		SettingsMasked:         masked,
		SettingsSet:            set,
	}
}

// ChannelsPageData is what templates/channels.html renders against.
type ChannelsPageData struct {
	PageData
	Channels     []channelRow
	Delivery     []deliveryRow
	QuietHours   string
	ChannelTypes []struct{ Value, Label string }
	NewModal     channelModalData
	EditModals   []channelModalData
	// TestResult, if non-empty, is rendered as a one-line status after a "Send test" action --
	// success or the error message.
	TestResult string
}

func buildChannelsPageData(r *http.Request, d Deps, testResult string) ChannelsPageData {
	cfg := d.Cfg()
	page := newPageData(r, d, "Notifications", "Where alerts are sent")

	newModal := newChannelModalData
	newModal.CSRF = page.CSRF
	modals := make([]channelModalData, 0, len(cfg.Channels))
	for _, cc := range cfg.Channels {
		m := channelModalFor(cc)
		m.CSRF = page.CSRF
		modals = append(modals, m)
	}
	sort.Slice(modals, func(i, j int) bool { return modals[i].Name < modals[j].Name })
	rows := channelRows(cfg)
	return ChannelsPageData{
		PageData:     page,
		Channels:     rows,
		Delivery:     deliveryRows(rows),
		QuietHours:   quietHoursText(cfg.QuietHours),
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

// channelSettingKeys lists, per channel type, which posted "settings.<k>" form fields
// channelSettingsFromForm reads.
var channelSettingKeys = map[string][]string{
	"telegram": {"token", "chat_id"},
	"email":    {"host", "port", "username", "password", "from", "to"},
	"ntfy":     {"server", "topic", "token"},
	"slack":    {"url"},
	"discord":  {"url"},
	"webhook":  {"url", "method", "template"},
	"gotify":   {"server", "token"},
}

// channelSecretSettingKeys maps each channel type to the settings key(s) that hold a
// credential -- the value buildNotifier.
var channelSecretSettingKeys = map[string][]string{
	"telegram": {"token"},
	"email":    {"password"},
	"webhook":  {"url"},
	"slack":    {"url"},
	"discord":  {"url"},
	"ntfy":     {"token"},
	"gotify":   {"token"},
}

// isSecretChannelSetting reports whether settings key k, for channel type
// typ, is a credential per channelSecretSettingKeys.
func isSecretChannelSetting(typ, k string) bool {
	for _, sk := range channelSecretSettingKeys[typ] {
		if sk == k {
			return true
		}
	}
	return false
}

// maskChannelSettings returns settings' template-safe counterpart: masked is a copy with
// every channelSecretSettingKeys value blanked out.
func maskChannelSettings(typ string, settings map[string]string) (masked map[string]string, set map[string]bool) {
	masked = make(map[string]string, len(settings))
	set = make(map[string]bool, len(settings))
	for k, v := range settings {
		set[k] = v != ""
		if isSecretChannelSetting(typ, k) {
			masked[k] = ""
			continue
		}
		masked[k] = v
	}
	return masked, set
}

// channelSettingsFromForm reads settings.<k> fields for typ from r.
func channelSettingsFromForm(r *http.Request, typ string) map[string]string {
	out := map[string]string{}
	for _, k := range channelSettingKeys[typ] {
		if v := strings.TrimSpace(r.FormValue("settings." + k)); v != "" {
			out[k] = v
			continue
		}
		if isSecretChannelSetting(typ, k) && r.FormValue("settings."+k+".clear") != "" {
			out[k] = ""
		}
	}
	return out
}

// applyChannelForm applies every posted channel field onto the named channel via
// config.Config.SetChannelField (reusing its existing validators, e.g. min_severity).
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

// validateDeliverable rejects an ENABLED channel that could not build a working notifier,
// so the web editor never silently persists a channel that delivery would drop.
func validateDeliverable(d Deps, cfg *config.Config, name string) error {
	if d.ValidateChannel == nil {
		return nil
	}
	cc, ok := cfg.GetChannel(name)
	if !ok || !cc.Enabled {
		return nil
	}
	return d.ValidateChannel(*cc, cfg)
}

// boolFormValue renders a posted boolean-ish field as "true"/"false" for
// config.SetChannelField's strconv.ParseBool-based enabled/ critical_overrides_quiet keys.
func boolFormValue(r *http.Request, field string) string {
	v := r.FormValue(field)
	if v == "" {
		return "false"
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return strconv.FormatBool(b)
	}
	// Present with some other truthy placeholder (e.g. a checkbox's value="1") -> checked.
	return "true"
}

// channelsAddHandler handles POST /channels: creates a new channel from the posted
// name/type/settings/routing fields.
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
		if err := validateDeliverable(d, newCfg, name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.API.ApplyConfig(newCfg); err != nil {
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

// channelsUpdateHandler handles POST /channels/{name}/update: applies the posted fields
// onto the EXISTING named channel, same validation/no-write contract as channelsAddHandler.
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
		if err := validateDeliverable(d, newCfg, name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.API.ApplyConfig(newCfg); err != nil {
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
		if err := d.API.ApplyConfig(newCfg); err != nil {
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

// channelsTestHandler handles POST /channels/{name}/test: sends a one-off test notification
// via Deps.API.TestChannel.
func channelsTestHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name, err := channelNameFromParam(r.PathValue("name"))
		if err != nil {
			http.Error(w, "invalid channel", http.StatusBadRequest)
			return
		}
		result := fmt.Sprintf("sent test notification via %q", name)
		if d.API == nil {
			result = "channel testing is not wired up in this build"
		} else if err := d.API.TestChannel(name); err != nil {
			result = fmt.Sprintf("test failed: %v", err)
		}

		data := buildChannelsPageData(r, d, result)
		if err := renderChannelsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
