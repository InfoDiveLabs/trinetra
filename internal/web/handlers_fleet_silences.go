// handlers_fleet_silences.go: GET /fleet/silences -- the silences (Active/Upcoming/Expired
// tabs, a create-silence form) and maintenance windows.
package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// --------------------------------------------------------------------------- Matcher rows:
// shared by the silence-create and maintenance-create forms.

// silenceMatcherRow is one OR'd matcher row (core.Matcher's four fields).
type silenceMatcherRow struct {
	Tag      string
	Node     string
	Rule     string
	Severity string
}

// parseMatcherRows reads prefix+"count" and prefix+"<i>_tag"/"node"/"rule"/ "severity" off
// r, rebuilding exactly what was posted.
func parseMatcherRows(r *http.Request, prefix string) []silenceMatcherRow {
	count, _ := strconv.Atoi(r.FormValue(prefix + "count"))
	rows := make([]silenceMatcherRow, 0, count)
	for i := 0; i < count; i++ {
		p := fmt.Sprintf("%s%d_", prefix, i)
		rows = append(rows, silenceMatcherRow{
			Tag:      r.FormValue(p + "tag"),
			Node:     r.FormValue(p + "node"),
			Rule:     r.FormValue(p + "rule"),
			Severity: r.FormValue(p + "severity"),
		})
	}
	if len(rows) == 0 {
		rows = []silenceMatcherRow{{}}
	}
	return rows
}

// applyMatcherRowOp reshapes rows in place for an "add_matcher"/ "remove_matcher:<i>" op
// token.
func applyMatcherRowOp(rows *[]silenceMatcherRow, op string) {
	parts := strings.Split(op, ":")
	switch parts[0] {
	case "add_matcher", "add_mnt_matcher":
		*rows = append(*rows, silenceMatcherRow{})
	case "remove_matcher", "remove_mnt_matcher":
		if i, ok := opIndexAt(parts, 1); ok && i < len(*rows) && len(*rows) > 1 {
			r := *rows
			*rows = append(r[:i], r[i+1:]...)
		}
	}
}

// matcherRowsToCore converts rows to core.Matchers, DROPPING an entirely blank row (every
// field empty) rather than posting it as a Matcher{}.
func matcherRowsToCore(rows []silenceMatcherRow) []core.Matcher {
	var out []core.Matcher
	for _, m := range rows {
		if m.Tag == "" && m.Node == "" && m.Rule == "" && m.Severity == "" {
			continue
		}
		out = append(out, core.Matcher{Tag: m.Tag, Node: m.Node, Rule: m.Rule, Severity: m.Severity})
	}
	return out
}

// matcherText renders one core.Matcher as "tag=x node=y rule=z severity=w"
// (only the set fields), for the silences/maintenance list's display.
func matcherText(m core.Matcher) string {
	var parts []string
	if m.Tag != "" {
		parts = append(parts, "tag="+m.Tag)
	}
	if m.Node != "" {
		parts = append(parts, "node="+m.Node)
	}
	if m.Rule != "" {
		parts = append(parts, "rule="+m.Rule)
	}
	if m.Severity != "" {
		parts = append(parts, "severity="+m.Severity)
	}
	return strings.Join(parts, " ")
}

// matchersText joins every OR'd matcher's own matcherText, parenthesized.
func matchersText(ms []core.Matcher) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, "("+matcherText(m)+")")
	}
	return strings.Join(parts, " OR ")
}

// buildSilenceNodeNames reads the roster through r's request-scoped fleetMemo
// (fleet_memo.go), exactly like buildAlertingTestNodeNames.
func buildSilenceNodeNames(r *http.Request, d Deps) []string {
	nodes, err := fleetMemoFrom(r).fleetNodes(d)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	return names
}

// --------------------------------------------------------------------------- Silence tabs:
// GET ?tab=active|upcoming|expired (a closed three-value enum).

type silenceTab string

const (
	silenceTabActive   silenceTab = "active"
	silenceTabUpcoming silenceTab = "upcoming"
	silenceTabExpired  silenceTab = "expired"
)

// parseSilenceTab reads ?tab= off r, defaulting to Active for anything absent/unrecognized.
func parseSilenceTab(r *http.Request) silenceTab {
	switch silenceTab(r.URL.Query().Get("tab")) {
	case silenceTabUpcoming:
		return silenceTabUpcoming
	case silenceTabExpired:
		return silenceTabExpired
	default:
		return silenceTabActive
	}
}

// parseSilencePage reads ?page= off r, normalizing an absent/invalid value
// to 1 -- mirrors parseIncidentQuery's own page handling.
func parseSilencePage(r *http.Request) int {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	return page
}

// paginateSilences slices all (already tab-filtered/sorted) into page's
// fleetIncidentsPageSize-row window, clamping page into [1, totalPages] first.
func paginateSilences(all []core.Silence, page int) (pageItems []core.Silence, totalPages, clampedPage int) {
	total := len(all)
	totalPages = (total + fleetIncidentsPageSize - 1) / fleetIncidentsPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * fleetIncidentsPageSize
	if start > total {
		start = total
	}
	end := start + fleetIncidentsPageSize
	if end > total {
		end = total
	}
	return all[start:end], totalPages, page
}

// SilenceTabLink is one of the three tab links.
type SilenceTabLink struct {
	Key    string
	Label  string
	Href   string
	Active bool
}

// silenceTabLinks builds the three tab links, current one marked Active.
func silenceTabLinks(tab silenceTab) []SilenceTabLink {
	order := []struct {
		key   silenceTab
		label string
	}{
		{silenceTabActive, "Active"},
		{silenceTabUpcoming, "Upcoming"},
		{silenceTabExpired, "Expired"},
	}
	out := make([]SilenceTabLink, 0, len(order))
	for _, t := range order {
		out = append(out, SilenceTabLink{Key: string(t.key), Label: t.label, Href: "/fleet/silences?tab=" + string(t.key), Active: t.key == tab})
	}
	return out
}

// filterSilencesByTab returns the subset of all matching tab as of now (unix seconds):
// active is Start<=now<End, upcoming is Start>now, expired is End<=now.
func filterSilencesByTab(all []core.Silence, tab silenceTab, now int64) []core.Silence {
	out := make([]core.Silence, 0, len(all))
	for _, s := range all {
		switch tab {
		case silenceTabUpcoming:
			if s.Start > now {
				out = append(out, s)
			}
		case silenceTabExpired:
			if s.End <= now {
				out = append(out, s)
			}
		default:
			if s.Start <= now && now < s.End {
				out = append(out, s)
			}
		}
	}
	switch tab {
	case silenceTabUpcoming:
		sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	case silenceTabExpired:
		sort.SliceStable(out, func(i, j int) bool { return out[i].End > out[j].End })
	default:
		sort.SliceStable(out, func(i, j int) bool { return out[i].End < out[j].End })
	}
	return out
}

// silenceTimeText renders a unix timestamp in the MASTER's own local zone (time.Local) with
// its abbreviation appended -- e.g. "2030-06-01 12:00 IST" -- "-" for <= 0.
func silenceTimeText(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	t := time.Unix(ts, 0).In(time.Local)
	abbr, _ := t.Zone()
	return t.Format("2006-01-02 15:04") + " " + abbr
}

// silenceTimeZoneNote is the "Times are in <IANA name> (<abbrev>)" note rendered next to
// the create-silence form's Start/Duration/End fields.
func silenceTimeZoneNote() string {
	now := time.Now()
	name := now.Location().String()
	abbr, _ := now.Zone()
	if name == "" || name == "Local" {
		return fmt.Sprintf("Times are in %s", abbr)
	}
	return fmt.Sprintf("Times are in %s (%s)", name, abbr)
}

// SilenceRow is one row of the silences list table.
type SilenceRow struct {
	ID           string
	MatchersText string
	StartText    string
	EndText      string
	Author       string
	Comment      string
	// CanExpire is false for an already-expired silence (the Expired tab) --
	// the template hides the Expire control entirely there.
	CanExpire bool
}

func newSilenceRow(s core.Silence, now int64) SilenceRow {
	return SilenceRow{
		ID:           s.ID,
		MatchersText: matchersText(s.Matchers),
		StartText:    silenceTimeText(s.Start),
		EndText:      silenceTimeText(s.End),
		Author:       s.Author,
		Comment:      s.Comment,
		CanExpire:    s.End > now,
	}
}

// datetimeLocalLayout is the exact format an <input type="datetime-local"> posts/expects
// (no seconds, no timezone offset -- the browser's own local wall clock).
const datetimeLocalLayout = "2006-01-02T15:04"

func formatDatetimeLocal(t time.Time) string {
	return t.In(time.Local).Format(datetimeLocalLayout)
}

func parseDatetimeLocal(s string) (time.Time, error) {
	return time.ParseInLocation(datetimeLocalLayout, s, time.Local)
}

// silenceDraft is the create-silence form's entire working copy.
type silenceDraft struct {
	Matchers []silenceMatcherRow
	StartAt  string // datetime-local; "" means "now" at submit time
	Duration string // one of incidentSilenceDurationChoices' keys, or "" when EndAt is used instead
	EndAt    string // datetime-local; when set, takes priority over Duration
	Comment  string
}

// newSilenceDraft is the fresh-GET default: one blank matcher row, Start
// defaulting to now, Duration defaulting to the shortest preset.
func newSilenceDraft() silenceDraft {
	return silenceDraft{
		Matchers: []silenceMatcherRow{{}},
		StartAt:  formatDatetimeLocal(time.Now()),
		Duration: incidentSilenceDurationChoices[0].key,
	}
}

func parseSilenceDraftForm(r *http.Request) silenceDraft {
	return silenceDraft{
		Matchers: parseMatcherRows(r, "matcher_"),
		StartAt:  r.FormValue("start_at"),
		Duration: r.FormValue("duration"),
		EndAt:    r.FormValue("end_at"),
		Comment:  r.FormValue("comment"),
	}
}

// SilenceDurationOption is one of the create form's duration <select>
// entries, plus the "use an explicit end instead" blank option.
type SilenceDurationOption struct {
	Key      string
	Label    string
	Selected bool
}

// silenceDurationOptions reuses incidentSilenceDurationChoices verbatim (handlers_fleet.go)
// -- the SAME 30m/1h/4h/24h presets the silence-from-incident form offers.
func silenceDurationOptions(selected string) []SilenceDurationOption {
	opts := make([]SilenceDurationOption, 0, len(incidentSilenceDurationChoices)+1)
	opts = append(opts, SilenceDurationOption{Key: "", Label: "(use explicit end below)", Selected: selected == ""})
	for _, c := range incidentSilenceDurationChoices {
		opts = append(opts, SilenceDurationOption{Key: c.key, Label: c.label, Selected: c.key == selected})
	}
	return opts
}

// resolveSilenceWindow computes the silence's Start/End from the draft: StartAt (or now, if
// blank), then EITHER EndAt.
func resolveSilenceWindow(d silenceDraft) (start, end int64, fieldErr string, err error) {
	start = time.Now().Unix()
	if strings.TrimSpace(d.StartAt) != "" {
		t, perr := parseDatetimeLocal(d.StartAt)
		if perr != nil {
			return 0, 0, "start", fmt.Errorf("invalid start date/time")
		}
		start = t.Unix()
	}
	if strings.TrimSpace(d.EndAt) != "" {
		t, perr := parseDatetimeLocal(d.EndAt)
		if perr != nil {
			return 0, 0, "end", fmt.Errorf("invalid end date/time")
		}
		return start, t.Unix(), "", nil
	}
	sec, ok := incidentSilenceDurationSeconds(d.Duration)
	if !ok {
		return 0, 0, "duration", fmt.Errorf("choose a duration, or set an explicit end time")
	}
	return start, start + sec, "", nil
}

// silenceErrField maps a CreateSilence rejection.
func silenceErrField(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "must match something"),
		strings.HasPrefix(msg, "invalid node glob"),
		strings.HasPrefix(msg, "invalid rule glob"):
		return "matchers"
	case strings.Contains(msg, "end must be after"):
		return "end"
	default:
		return ""
	}
}

// commonTZChoices is a fixed list of common IANA zone names for the maintenance
// form's TZ <select>.
var commonTZChoices = []string{
	"UTC",
	"America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
	"America/Sao_Paulo", "Europe/London", "Europe/Paris", "Europe/Berlin", "Europe/Moscow",
	"Africa/Cairo", "Africa/Johannesburg",
	"Asia/Dubai", "Asia/Kolkata", "Asia/Shanghai", "Asia/Singapore", "Asia/Tokyo",
	"Australia/Sydney", "Pacific/Auckland",
}

// masterLocalTZName returns this daemon's own local IANA zone name (e.g. set via the TZ
// environment variable, or the system zone).
func masterLocalTZName() string {
	name := time.Now().Location().String()
	if name == "" || name == "Local" || name == "UTC" {
		return ""
	}
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}

// tzSelectOptions builds the TZ <select>'s options: the master's own local zone
// first, UTC, then every other common zone, each listed at most once.
func tzSelectOptions() []string {
	local := masterLocalTZName()
	seen := map[string]bool{}
	out := make([]string, 0, len(commonTZChoices)+1)
	if local != "" {
		out = append(out, local)
		seen[local] = true
	}
	for _, z := range commonTZChoices {
		if seen[z] {
			continue
		}
		seen[z] = true
		out = append(out, z)
	}
	return out
}

// maintenanceDraft is the create-maintenance form's entire working copy.
type maintenanceDraft struct {
	Name     string
	Matchers []silenceMatcherRow
	Weekdays []int
	From     string
	To       string
	TZSelect string
	TZCustom string
}

// newMaintenanceDraft is the fresh-GET default: one blank matcher row, no weekdays checked,
// blank From/To, TZ defaulting to the master's own local zone.
func newMaintenanceDraft() maintenanceDraft {
	tz := masterLocalTZName()
	if tz == "" {
		tz = "UTC"
	}
	return maintenanceDraft{Matchers: []silenceMatcherRow{{}}, TZSelect: tz}
}

// WeekdayChecked reports whether day (0=Sunday..6=Saturday) is checked in
// this draft -- the weekday checkboxes' own checked-state helper.
func (d maintenanceDraft) WeekdayChecked(day int) bool {
	for _, w := range d.Weekdays {
		if w == day {
			return true
		}
	}
	return false
}

// ResolvedTZ is the TZ value that would actually be saved: TZCustom
// (trimmed) when non-blank, else TZSelect.
func (d maintenanceDraft) ResolvedTZ() string {
	if custom := strings.TrimSpace(d.TZCustom); custom != "" {
		return custom
	}
	return d.TZSelect
}

func parseMaintenanceDraftForm(r *http.Request) maintenanceDraft {
	d := maintenanceDraft{
		Name:     r.FormValue("name"),
		Matchers: parseMatcherRows(r, "mnt_matcher_"),
		From:     r.FormValue("from"),
		To:       r.FormValue("to"),
		TZSelect: r.FormValue("tz_select"),
		TZCustom: r.FormValue("tz_custom"),
	}
	for _, s := range r.Form["weekday"] {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 && n <= 6 {
			d.Weekdays = append(d.Weekdays, n)
		}
	}
	sort.Ints(d.Weekdays)
	return d
}

// weekdayAbbrev is Sun..Sat, index == time.Weekday's own int value.
var weekdayAbbrev = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

// WeekdayOption is one of the maintenance form's seven weekday checkboxes -- built
// server-side.
type WeekdayOption struct {
	Day     int
	Label   string
	Checked bool
}

func weekdayOptions(d maintenanceDraft) []WeekdayOption {
	opts := make([]WeekdayOption, 0, len(weekdayAbbrev))
	for day, label := range weekdayAbbrev {
		opts = append(opts, WeekdayOption{Day: day, Label: label, Checked: d.WeekdayChecked(day)})
	}
	return opts
}

func weekdaysText(days []int) string {
	labels := make([]string, 0, len(days))
	for _, d := range days {
		if d >= 0 && d <= 6 {
			labels = append(labels, weekdayAbbrev[d])
		}
	}
	return strings.Join(labels, ",")
}

// maintenanceNextText renders m's next occurrence.
func maintenanceNextText(m core.Maintenance) string {
	occ, ok := core.NextMaintenanceOccurrence(m, time.Now())
	if !ok {
		return "-"
	}
	loc, err := time.LoadLocation(m.TZ)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().Unix()
	if occ.Start <= now && now < occ.End {
		return "active now, until " + time.Unix(occ.End, 0).In(loc).Format("Jan 2 15:04 MST")
	}
	return time.Unix(occ.Start, 0).In(loc).Format("Mon Jan 2 15:04 MST")
}

// MaintenanceRow is one row of the maintenance windows list table.
type MaintenanceRow struct {
	ID           string
	Name         string
	MatchersText string
	WeekdaysText string
	From, To, TZ string
	Author       string
	NextText     string
}

func newMaintenanceRow(m core.Maintenance) MaintenanceRow {
	return MaintenanceRow{
		ID:           m.ID,
		Name:         m.Name,
		MatchersText: matchersText(m.Matchers),
		WeekdaysText: weekdaysText(m.Weekdays),
		From:         m.From,
		To:           m.To,
		TZ:           m.TZ,
		Author:       m.Author,
		NextText:     maintenanceNextText(m),
	}
}

// maintenanceErrField maps a SaveMaintenance rejection.
func maintenanceErrField(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "must match something"),
		strings.HasPrefix(msg, "invalid node glob"),
		strings.HasPrefix(msg, "invalid rule glob"):
		return "matchers"
	case strings.Contains(msg, "needs a name"):
		return "name"
	case strings.Contains(msg, "needs at least one weekday"),
		strings.HasPrefix(msg, "invalid weekday"):
		return "weekdays"
	case strings.Contains(msg, "--from"):
		return "from"
	case strings.Contains(msg, "--to"):
		return "to"
	case strings.Contains(msg, "--tz"):
		return "tz"
	default:
		return ""
	}
}

// --------------------------------------------------------------------------- Flash: a
// FIXED set of codes only (like resolveIncidentFlash).

func resolveSilencesFlash(r *http.Request) (text string, isErr bool) {
	switch r.URL.Query().Get("flash") {
	case "silence_created":
		return "Silence created", false
	case "silence_expired":
		return "Silence expired", false
	case "maintenance_saved":
		return "Maintenance window saved", false
	case "maintenance_deleted":
		return "Maintenance window deleted", false
	}
	return "", false
}

// --------------------------------------------------------------------------- Page data
// ---------------------------------------------------------------------------

// SilencesPageData is what templates/fleet_silences.html's "content" block renders against.
type SilencesPageData struct {
	PageData

	Tab      silenceTab
	TabLinks []SilenceTabLink

	Silences   []SilenceRow
	Page       int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevHref   string
	NextHref   string

	SilenceDraft           silenceDraft
	SilenceDurationOptions []SilenceDurationOption
	// TZNote is the "Times are in <IANA name> (<abbrev>)" note rendered next to the
	// Start/Duration/End fields, so an admin knows which zone every silence time on this page.
	TZNote          string
	SilenceErr      string
	SilenceErrField string

	Maintenances []MaintenanceRow

	MaintenanceDraft    maintenanceDraft
	WeekdayOptions      []WeekdayOption
	TZOptions           []string
	MaintenanceErr      string
	MaintenanceErrField string

	NodeNames []string

	Flash    string
	FlashErr bool
}

// silencesPageOptions is buildSilencesPageData's input: the page's transient,
// this-response-only state.
type silencesPageOptions struct {
	Tab silenceTab

	SilenceLoaded   bool
	Silence         silenceDraft
	SilenceErr      string
	SilenceErrField string

	MaintenanceLoaded   bool
	Maintenance         maintenanceDraft
	MaintenanceErr      string
	MaintenanceErrField string

	Flash    string
	FlashErr bool
}

func buildSilencesPageData(r *http.Request, d Deps, opts silencesPageOptions) SilencesPageData {
	var allSilences []core.Silence
	var allMaint []core.Maintenance
	if fleet, err := fleetAPIFor(d); err == nil {
		allSilences, _ = fleet.Silences()
		allMaint, _ = fleet.Maintenances()
	}
	now := time.Now().Unix()

	tabbed := filterSilencesByTab(allSilences, opts.Tab, now)
	pageItems, totalPages, page := paginateSilences(tabbed, parseSilencePage(r))
	rows := make([]SilenceRow, 0, len(pageItems))
	for _, s := range pageItems {
		rows = append(rows, newSilenceRow(s, now))
	}
	pageHref := func(p int) string {
		return "/fleet/silences?tab=" + string(opts.Tab) + "&page=" + strconv.Itoa(p)
	}

	// Maintenance windows are NOT paginated -- see paginateSilences' doc for the rationale.
	mrows := make([]MaintenanceRow, 0, len(allMaint))
	for _, m := range allMaint {
		mrows = append(mrows, newMaintenanceRow(m))
	}
	sort.SliceStable(mrows, func(i, j int) bool { return mrows[i].Name < mrows[j].Name })

	sDraft := opts.Silence
	if !opts.SilenceLoaded {
		sDraft = newSilenceDraft()
	}
	mDraft := opts.Maintenance
	if !opts.MaintenanceLoaded {
		mDraft = newMaintenanceDraft()
	}

	flash, flashErr := opts.Flash, opts.FlashErr
	if flash == "" {
		flash, flashErr = resolveSilencesFlash(r)
	}

	return SilencesPageData{
		PageData:               newPageData(r, d, "Silences", "Silences and maintenance windows"),
		Tab:                    opts.Tab,
		TabLinks:               silenceTabLinks(opts.Tab),
		Silences:               rows,
		Page:                   page,
		TotalPages:             totalPages,
		HasPrev:                page > 1,
		HasNext:                page < totalPages,
		PrevHref:               pageHref(page - 1),
		NextHref:               pageHref(page + 1),
		SilenceDraft:           sDraft,
		SilenceDurationOptions: silenceDurationOptions(sDraft.Duration),
		TZNote:                 silenceTimeZoneNote(),
		SilenceErr:             opts.SilenceErr,
		SilenceErrField:        opts.SilenceErrField,
		Maintenances:           mrows,
		MaintenanceDraft:       mDraft,
		WeekdayOptions:         weekdayOptions(mDraft),
		TZOptions:              tzSelectOptions(),
		MaintenanceErr:         opts.MaintenanceErr,
		MaintenanceErrField:    opts.MaintenanceErrField,
		NodeNames:              buildSilenceNodeNames(r, d),
		Flash:                  flash,
		FlashErr:               flashErr,
	}
}

// --------------------------------------------------------------------------- Rendering
// ---------------------------------------------------------------------------

func renderSilencesPage(w http.ResponseWriter, data SilencesPageData, status int) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/fleet_silences.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// renderSilencesError re-renders the full page with a top-level flash (FlashErr=true) at
// the given 4xx status (FleetAPI errors render as a flash, never a 500).
func renderSilencesError(w http.ResponseWriter, r *http.Request, d Deps, tab silenceTab, msg string, status int) {
	data := buildSilencesPageData(r, d, silencesPageOptions{Tab: tab, Flash: msg, FlashErr: true})
	if err := renderSilencesPage(w, data, status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// redirectToSilences redirects to GET /fleet/silences with a fixed ?flash= code and a
// server-computed ?tab= -- both closed enums, never anything the client posted.
func redirectToSilences(w http.ResponseWriter, r *http.Request, tab silenceTab, flashCode string) {
	v := url.Values{"tab": {string(tab)}}
	if flashCode != "" {
		v.Set("flash", flashCode)
	}
	http.Redirect(w, r, "/fleet/silences?"+v.Encode(), http.StatusSeeOther)
}

// --------------------------------------------------------------------------- Handlers
// ---------------------------------------------------------------------------

// fleetSilencesPageHandler serves GET /fleet/silences: viewer+.
func fleetSilencesPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		data := buildSilencesPageData(r, d, silencesPageOptions{Tab: parseSilenceTab(r)})
		if err := renderSilencesPage(w, data, http.StatusOK); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// fleetSilenceCreateHandler serves POST /fleet/silences (admin+CSRF, fleetAdminMutation).
func fleetSilenceCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		tab := parseSilenceTab(r)
		if err := r.ParseForm(); err != nil {
			renderSilencesError(w, r, d, tab, "invalid form", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderSilencesError(w, r, d, tab, "fleet not available", http.StatusNotFound)
			return
		}
		draft := parseSilenceDraftForm(r)
		if op := r.FormValue("op"); op != "" && op != "save" {
			applyMatcherRowOp(&draft.Matchers, op)
			data := buildSilencesPageData(r, d, silencesPageOptions{Tab: tab, Silence: draft, SilenceLoaded: true})
			if rerr := renderSilencesPage(w, data, http.StatusOK); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		start, end, fieldErr, werr := resolveSilenceWindow(draft)
		if werr != nil {
			data := buildSilencesPageData(r, d, silencesPageOptions{
				Tab: tab, Silence: draft, SilenceLoaded: true,
				SilenceErr: werr.Error(), SilenceErrField: fieldErr,
			})
			if rerr := renderSilencesPage(w, data, http.StatusBadRequest); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		actor := auditUser(r)
		created, cerr := fleet.CreateSilence(core.Silence{
			Matchers: matcherRowsToCore(draft.Matchers),
			Start:    start, End: end,
			Author:  actor,
			Comment: draft.Comment,
		})
		if cerr != nil {
			data := buildSilencesPageData(r, d, silencesPageOptions{
				Tab: tab, Silence: draft, SilenceLoaded: true,
				SilenceErr: cerr.Error(), SilenceErrField: silenceErrField(cerr),
			})
			if rerr := renderSilencesPage(w, data, fleetAPIErrStatus(cerr)); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}
		logAudit(d, r, "fleet.silence.create", created.ID, "", actor)
		landing := silenceTabActive
		if created.Start > time.Now().Unix() {
			landing = silenceTabUpcoming
		}
		redirectToSilences(w, r, landing, "silence_created")
	}
}

// fleetSilenceExpireHandler serves POST /fleet/silences/{id}/expire (admin+CSRF,
// fleetAdminMutation), used with the in-page two-step confirm.
func fleetSilenceExpireHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		tab := parseSilenceTab(r)
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderSilencesError(w, r, d, tab, "fleet not available", http.StatusNotFound)
			return
		}
		id := r.PathValue("id")
		actor := auditUser(r)
		if err := fleet.ExpireSilence(id, actor); err != nil {
			renderSilencesError(w, r, d, tab, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.silence.expire", id, "", actor)
		redirectToSilences(w, r, tab, "silence_expired")
	}
}

// fleetMaintenanceCreateHandler serves POST /fleet/maintenance (admin+CSRF,
// fleetAdminMutation).
func fleetMaintenanceCreateHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		tab := parseSilenceTab(r)
		if err := r.ParseForm(); err != nil {
			renderSilencesError(w, r, d, tab, "invalid form", http.StatusBadRequest)
			return
		}
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderSilencesError(w, r, d, tab, "fleet not available", http.StatusNotFound)
			return
		}
		draft := parseMaintenanceDraftForm(r)
		if op := r.FormValue("op"); op != "" && op != "save_maintenance" {
			applyMatcherRowOp(&draft.Matchers, op)
			data := buildSilencesPageData(r, d, silencesPageOptions{Tab: tab, Maintenance: draft, MaintenanceLoaded: true})
			if rerr := renderSilencesPage(w, data, http.StatusOK); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}

		actor := auditUser(r)
		saved, serr := fleet.SaveMaintenance(core.Maintenance{
			Name:     strings.TrimSpace(draft.Name),
			Matchers: matcherRowsToCore(draft.Matchers),
			Weekdays: draft.Weekdays,
			From:     draft.From,
			To:       draft.To,
			TZ:       draft.ResolvedTZ(),
			Author:   actor,
		})
		if serr != nil {
			data := buildSilencesPageData(r, d, silencesPageOptions{
				Tab: tab, Maintenance: draft, MaintenanceLoaded: true,
				MaintenanceErr: serr.Error(), MaintenanceErrField: maintenanceErrField(serr),
			})
			if rerr := renderSilencesPage(w, data, fleetAPIErrStatus(serr)); rerr != nil {
				http.Error(w, rerr.Error(), http.StatusInternalServerError)
			}
			return
		}
		logAudit(d, r, "fleet.maintenance.save", saved.ID, "", actor)
		redirectToSilences(w, r, tab, "maintenance_saved")
	}
}

// fleetMaintenanceDeleteHandler serves POST /fleet/maintenance/{id}/delete (admin+CSRF,
// fleetAdminMutation).
func fleetMaintenanceDeleteHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fleetGateHTML(w, r, d) {
			return
		}
		tab := parseSilenceTab(r)
		fleet, err := fleetAPIFor(d)
		if err != nil {
			renderSilencesError(w, r, d, tab, "fleet not available", http.StatusNotFound)
			return
		}
		id := r.PathValue("id")
		actor := auditUser(r)
		if err := fleet.DeleteMaintenance(id, actor); err != nil {
			renderSilencesError(w, r, d, tab, err.Error(), fleetAPIErrStatus(err))
			return
		}
		logAudit(d, r, "fleet.maintenance.delete", id, "", actor)
		redirectToSilences(w, r, tab, "maintenance_deleted")
	}
}
