//go:build web

package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"serverwatch/internal/config"
)

// configMutation composes requireRole(RoleAdmin, ...) with requireCSRF,
// mirroring usersMutation (handlers_users.go): only an admin session may
// POST /config, and only with a valid CSRF token.
func configMutation(next http.HandlerFunc) http.HandlerFunc {
	return requireRole(RoleAdmin, func(w http.ResponseWriter, r *http.Request) {
		requireCSRF(next).ServeHTTP(w, r)
	})
}

// cloneConfig returns a deep, independent copy of c via a JSON round-trip:
// simplest way to get a copy whose map/slice fields (Targets, Channels,
// Collect.*) don't alias the original, so validating a batch of edits
// against the clone can never mutate the live config before every field has
// passed — the "bad value -> 400, no write" requirement (routes.go/
// configSaveHandler) depends on this.
func cloneConfig(c *config.Config) (*config.Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	nc := &config.Config{}
	if err := json.Unmarshal(b, nc); err != nil {
		return nil, err
	}
	return nc, nil
}

// trimFloatText formats v the same way config.Config.Get does internally
// (strconv.FormatFloat(v, 'f', -1, 64)) — used for both rendering a
// TargetOverride.Threshold into the monitors table and diffing old/new
// values for the audit log.
func trimFloatText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// configTargetRow is one row of the /config "What to monitor" table.
type configTargetRow struct {
	Name      string
	Enabled   bool
	Threshold string // "" = "use the global threshold above"
}

// configTargetRows merges cfg.Targets (explicit per-target overrides, the
// only source of "docker:...", "smart:...", "temp:..." style targets today)
// with the disk mounts the live snapshot currently reports (Deps.Snapshot),
// so a disk nobody has touched yet still shows up as an enabled,
// default-threshold row rather than not appearing at all.
//
// This is NOT the full auto-discovered target list the mockup's config.html
// shows (disk + docker + smart + temp, sourced from
// internal/serverwatch/discover.go's Discover): internal/web must not import
// internal/serverwatch (see the design note atop
// internal/serverwatch/web_deps.go), and Deps doesn't expose a generic
// target inventory today — only Snapshot's disk mounts and whatever
// overrides already exist in config. Extending Deps with a full target list
// is future work; scoping to what's actually available here keeps this
// task's "enable/disable + per-target threshold" contract real and testable
// without widening the serverwatch/web seam further.
func configTargetRows(cfg *config.Config, snap DashboardView) []configTargetRow {
	seen := map[string]bool{}
	var out []configTargetRow
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		row := configTargetRow{Name: name, Enabled: cfg.TargetEnabled(name)}
		if v, ok := cfg.TargetThreshold(name); ok {
			row.Threshold = trimFloatText(v)
		}
		out = append(out, row)
	}
	for _, dv := range snap.Disks {
		add("disk:" + dv.Mount)
	}
	names := make([]string, 0, len(cfg.Targets))
	for name := range cfg.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		add(name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clearTargetThreshold removes a target's threshold override (if any),
// leaving its Disabled flag untouched — the config package itself only
// exposes SetTargetThreshold (always sets), not a way to clear one, since
// the CLI (`serverwatch target threshold ... `, if it existed) has never
// needed to; the web form does, since leaving the "Alert at" cell blank
// means "use the global threshold above" (configTargetRow.Threshold == "").
func clearTargetThreshold(c *config.Config, name string) {
	if c.Targets == nil {
		return
	}
	o, ok := c.Targets[name]
	if !ok {
		return
	}
	o.Threshold = nil
	c.Targets[name] = o
}

// parseQuietHours splits cfg.QuietHours ("H-H"/"HH-HH", validateQuietHours's
// format — see internal/config/config.go) into from/to hour integers plus
// whether quiet hours are enabled at all (QuietHours != ""). An unparseable
// stored value (shouldn't happen — Set already validates on write) falls
// back to 23/8, the mockup's own default, rather than failing the page.
func parseQuietHours(s string) (from, to int, enabled bool) {
	if s == "" {
		return 23, 8, false
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 23, 8, true
	}
	f, err1 := strconv.Atoi(parts[0])
	t, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 23, 8, true
	}
	return f, t, true
}

// weekdays is the fixed set schedule.weekly's "dow@HH:MM" format allows
// (config.go's daysOfWeek), in display order for the <select>.
var weekdays = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// parseWeekly splits cfg.Schedule.Weekly ("dow@HH:MM") into day/time plus
// whether it's enabled at all (Weekly != ""), mirroring parseQuietHours.
func parseWeekly(s string) (day, hm string, enabled bool) {
	if s == "" {
		return "mon", "09:00", false
	}
	parts := strings.SplitN(s, "@", 2)
	if len(parts) != 2 {
		return "mon", "09:00", true
	}
	return parts[0], parts[1], true
}

// ConfigPageData is what templates/config.html renders against.
type ConfigPageData struct {
	PageData
	DiskPct       string
	TempC         string
	MemPct        string
	AnomalySigma  string
	QuietEnabled  bool
	QuietFrom     int
	QuietTo       int
	QuietHours    []int // 0..23, for the From/To <select> options
	DailyEnabled  bool
	DailyTime     string
	WeeklyEnabled bool
	WeeklyDay     string
	WeeklyTime    string
	Weekdays      []string
	DeadmanURL    string
	Targets       []configTargetRow
}

// buildConfigPageData assembles ConfigPageData from the current config
// (d.Cfg()) and the live snapshot's disk mounts (d.Snapshot, for
// configTargetRows).
func buildConfigPageData(r *http.Request, d Deps) ConfigPageData {
	cfg := d.Cfg()
	var snap DashboardView
	if d.Snapshot != nil {
		snap = d.Snapshot()
	}
	qFrom, qTo, qOn := parseQuietHours(cfg.QuietHours)
	wDay, wTime, wOn := parseWeekly(cfg.Schedule.Weekly)
	hours := make([]int, 24)
	for i := range hours {
		hours[i] = i
	}
	return ConfigPageData{
		PageData:      newPageData(r, "Configuration", "Thresholds, monitors, schedules, quiet hours", "ok"),
		DiskPct:       trimFloatText(cfg.Thresholds.DiskPct),
		TempC:         trimFloatText(cfg.Thresholds.TempC),
		MemPct:        trimFloatText(cfg.Thresholds.MemPct),
		AnomalySigma:  trimFloatText(cfg.BaselineSigma),
		QuietEnabled:  qOn,
		QuietFrom:     qFrom,
		QuietTo:       qTo,
		QuietHours:    hours,
		DailyEnabled:  cfg.Schedule.Daily != "",
		DailyTime:     firstNonEmpty(cfg.Schedule.Daily, "09:00"),
		WeeklyEnabled: wOn,
		WeeklyDay:     wDay,
		WeeklyTime:    wTime,
		Weekdays:      weekdays,
		DeadmanURL:    cfg.Healthchecks.URL,
		Targets:       configTargetRows(cfg, snap),
	}
}

func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// renderConfigPage renders templates/config.html through the full app-shell
// layout, mirroring renderUsersPage (handlers_users.go).
func renderConfigPage(w http.ResponseWriter, data ConfigPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/config.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// configPageHandler renders GET /config. requireRole(RoleAdmin, ...)
// (routes.go's wiring) has already gated this by the time it runs.
func configPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildConfigPageData(r, d)
		if err := renderConfigPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// scalarEdit is one dotted config.Set key/value pair this handler applies,
// paired up so configSaveHandler can both validate (config.Config.Set
// itself) and, for whatever actually changed, emit an audit record.
type scalarEdit struct{ key, val string }

// quietHoursFormValue composes the posted quiet-hours enabled flag + from/to
// hour <select> values into the "H-H" string config.Set("quiet_hours", ...)
// expects, or "" to clear it — the inverse of parseQuietHours.
func quietHoursFormValue(r *http.Request) string {
	if r.FormValue("quiet_enabled") == "" {
		return ""
	}
	return r.FormValue("quiet_from") + "-" + r.FormValue("quiet_to")
}

// dailyFormValue composes the posted daily-digest enabled flag + time into
// schedule.daily's "" or "HH:MM" form.
func dailyFormValue(r *http.Request) string {
	if r.FormValue("daily_enabled") == "" {
		return ""
	}
	return r.FormValue("daily_time")
}

// weeklyFormValue composes the posted weekly-rollup enabled flag + day/time
// into schedule.weekly's "" or "dow@HH:MM" form.
func weeklyFormValue(r *http.Request) string {
	if r.FormValue("weekly_enabled") == "" {
		return ""
	}
	return r.FormValue("weekly_day") + "@" + r.FormValue("weekly_time")
}

// applyTargetEdits applies the posted monitors-table rows (target_name[],
// target_enabled[] — only checked boxes are present, matched by value since
// unchecked checkboxes never submit — and target_threshold[], positionally
// paired with target_name[] since a text input always submits) onto newCfg.
// Returns a 400-worthy error on an unparseable threshold; never partially
// applies past that point's caller responsibility (newCfg is always a
// clone — see cloneConfig — so an error here still leaves the live config
// untouched).
func applyTargetEdits(newCfg *config.Config, r *http.Request) error {
	names := r.Form["target_name"]
	thresholds := r.Form["target_threshold"]
	enabledSet := map[string]bool{}
	for _, n := range r.Form["target_enabled"] {
		enabledSet[n] = true
	}
	for i, name := range names {
		if name == "" {
			continue
		}
		newCfg.SetTarget(name, enabledSet[name])
		thr := ""
		if i < len(thresholds) {
			thr = strings.TrimSpace(thresholds[i])
		}
		if thr == "" {
			clearTargetThreshold(newCfg, name)
			continue
		}
		v, err := strconv.ParseFloat(thr, 64)
		if err != nil {
			return fmt.Errorf("target %q threshold %q invalid: want a number", name, thr)
		}
		newCfg.SetTargetThreshold(name, v)
	}
	return nil
}

// configSaveHandler handles POST /config: validates every posted field
// against a clone of the current config via config.Config.Set (its
// existing, already-tested validators — a bad value rejects with 400 and
// writes nothing), and only once every field has passed persists +
// in-process applies via Deps.Reload, then appends one audit record per
// scalar field that actually changed plus one summarizing any monitors
// table changes.
func configSaveHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		oldCfg := d.Cfg()
		newCfg, err := cloneConfig(oldCfg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		edits := []scalarEdit{
			{"thresholds.disk_pct", r.FormValue("disk_pct")},
			{"thresholds.temp_c", r.FormValue("temp_c")},
			{"thresholds.mem_pct", r.FormValue("mem_pct")},
			{"baseline_sigma", r.FormValue("anomaly_sigma")},
			{"quiet_hours", quietHoursFormValue(r)},
			{"schedule.daily", dailyFormValue(r)},
			{"schedule.weekly", weeklyFormValue(r)},
			{"healthchecks.url", r.FormValue("deadman_url")},
		}
		for _, e := range edits {
			if err := newCfg.Set(e.key, e.val); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := applyTargetEdits(newCfg, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := d.Reload(newCfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		targetsChanged := false
		for _, name := range r.Form["target_name"] {
			if name == "" {
				continue
			}
			oldEnabled := oldCfg.TargetEnabled(name)
			newEnabled := newCfg.TargetEnabled(name)
			oldThr, oldOK := oldCfg.TargetThreshold(name)
			newThr, newOK := newCfg.TargetThreshold(name)
			if oldEnabled != newEnabled || oldOK != newOK || (oldOK && newOK && oldThr != newThr) {
				targetsChanged = true
				break
			}
		}
		if targetsChanged {
			logAudit(d, r, "config.set", "targets", "", "monitors table updated")
		}
		for _, e := range edits {
			old, _ := oldCfg.Get(e.key)
			new, _ := newCfg.Get(e.key)
			if old != new {
				logAudit(d, r, "config.set", e.key, old, new)
			}
		}

		data := buildConfigPageData(r, d)
		if err := renderConfigPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
