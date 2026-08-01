package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// alertLogEvent mirrors serverwatch.AlertEvent's JSON encoding just enough
// to decode it (alertlog.go's AlertEvent/Delivery), the same "decode the
// JSON shape, not the Go type" pattern activeAlertView/alertStateFile
// (handlers_dashboard.go) already use for AlertState — see that file's doc
// for why this doesn't create an import-cycle risk.
type alertLogEvent struct {
	Time      int64  `json:"time"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Severity  string `json:"severity"`
	Kind      string `json:"kind"` // "fire" | "recover"
	Source    string `json:"source"`
	Delivered []struct {
		Channel string `json:"channel"`
		OK      bool   `json:"ok"`
		Err     string `json:"err,omitempty"`
	} `json:"delivered,omitempty"`
}

// loadAlertLogEvents reads and decodes every line of path (Deps.AlertLogPath,
// an append-only JSONL file — see alertlog.go's AlertLog) into
// []alertLogEvent, newest first. A missing file, an empty path (not
// configured, e.g. some tests), or any decode error along the way all
// degrade to "as many valid events as were found" rather than a 500 —
// individual malformed lines are skipped (mirroring AlertLog.
// AlertEventsSince's own tolerance for corrupt lines), and a totally
// unreadable/garbage file just yields an empty list. This page is
// display-only history, never a source of truth serverwatch itself depends
// on, so silently degrading is the right failure mode.
func loadAlertLogEvents(path string) []alertLogEvent {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []alertLogEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev alertLogEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// alertHistoryRow is one row of the /alerts "Recent history" table.
type alertHistoryRow struct {
	Severity  string
	Title     string
	Source    string
	Kind      string // "fire" | "recover"
	When      string
	Delivered string // comma-joined channel names that accepted delivery
}

// deliveredNames renders an alertLogEvent's Delivered slice as a
// comma-joined list of channels that actually accepted the notification
// (OK == true), or "—" if none did (or none were configured).
func deliveredNames(ev alertLogEvent) string {
	var names []string
	for _, d := range ev.Delivered {
		if d.OK {
			names = append(names, d.Channel)
		}
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, ", ")
}

// alertHistoryRows caps the log to the most recent maxAlertHistoryRows
// events (newest first, already loadAlertLogEvents's order) for the
// "Recent history" table — the mockup shows a bounded recent window, not
// the entire log.
const maxAlertHistoryRows = 100

func alertHistoryRows(events []alertLogEvent) []alertHistoryRow {
	if len(events) > maxAlertHistoryRows {
		events = events[:maxAlertHistoryRows]
	}
	out := make([]alertHistoryRow, 0, len(events))
	for _, ev := range events {
		out = append(out, alertHistoryRow{
			Severity:  ev.Severity,
			Title:     ev.Title,
			Source:    ev.Source,
			Kind:      ev.Kind,
			When:      time.Unix(ev.Time, 0).UTC().Format("Jan 2 15:04"),
			Delivered: deliveredNames(ev),
		})
	}
	return out
}

// activeAlertRow is one row of the /alerts "Firing" table: activeAlertView
// (handlers_dashboard.go) plus the display/URL bits this page needs beyond
// the dashboard panel's simpler use of it.
type activeAlertRow struct {
	activeAlertView
	Since string // human "Xm ago" / "Xh ago"
	Param string // credentialParam([]byte(Key)), for the Ack form's URL
}

// agoText renders a Unix timestamp as a short "Xm ago"/"Xh ago"/"Xd ago"
// duration relative to now, mirroring the mockup's "14m ago" style.
func agoText(unixSec int64) string {
	d := time.Since(time.Unix(unixSec, 0))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func activeAlertRows(active []activeAlertView) []activeAlertRow {
	out := make([]activeAlertRow, 0, len(active))
	for _, a := range active {
		out = append(out, activeAlertRow{
			activeAlertView: a,
			Since:           agoText(a.Since),
			Param:           credentialParam([]byte(a.Key)),
		})
	}
	return out
}

// AlertsPageData is what templates/alerts.html renders against.
//
// NOTE: this cannot be named "Active" — PageData already declares an
// Active string field (the current request path, for base.html's nav
// highlighting via {{eq .Href $.Active}}), and an explicitly declared field
// at depth 0 SHADOWS an embedded field of the same name at depth 1, which
// would silently break every page's nav "active" link once this struct
// embeds PageData.
type AlertsPageData struct {
	PageData
	ActiveAlerts []activeAlertRow
	History      []alertHistoryRow
	FiringCount  int
	AckedCount   int
	Resolved7d   int
	UptimePct30d float64
	HasUptime    bool
}

// resolvedInWindow counts "recover" events within the last window (relative
// to now) — the mockup's "Resolved · 7d" tile.
func resolvedInWindow(events []alertLogEvent, window time.Duration) int {
	cutoff := time.Now().Add(-window).Unix()
	n := 0
	for _, ev := range events {
		if ev.Kind == "recover" && ev.Time >= cutoff {
			n++
		}
	}
	return n
}

// uptimePct30d computes the mockup's "Uptime · 30d" tile from Deps.Events
// (the same downtime event store the history page's panel uses,
// events_store.go): 100% minus the fraction of the last 30 days spent in a
// downtime event. Returns (0, false) when d.Events is nil (store-writes-
// disabled mode, or a test Deps that doesn't wire one) so the caller can
// render "—" instead of a misleading 100%.
func uptimePct30d(d Deps) (float64, bool) {
	if d.Events == nil {
		return 0, false
	}
	const window = 30 * 24 * time.Hour
	to := time.Now().Unix()
	from := time.Now().Add(-window).Unix()
	evs, err := d.Events.Events(from, to)
	if err != nil {
		return 0, false
	}
	var down int64
	for _, e := range evs {
		down += e.DurationSec
	}
	total := int64(window / time.Second)
	if total <= 0 {
		return 0, false
	}
	pct := 100 * (1 - float64(down)/float64(total))
	if pct < 0 {
		pct = 0
	}
	return pct, true
}

func buildAlertsPageData(r *http.Request, d Deps) AlertsPageData {
	active := loadActiveAlerts(d.AlertStatePath)
	events := loadAlertLogEvents(d.AlertLogPath)

	firing, acked := 0, 0
	for _, a := range active {
		if a.Acked {
			acked++
		} else {
			firing++
		}
	}
	uptime, hasUptime := uptimePct30d(d)

	return AlertsPageData{
		PageData:     newPageData(r, d, "Alerts & incidents", "Firing now + history"),
		ActiveAlerts: activeAlertRows(active),
		History:      alertHistoryRows(events),
		FiringCount:  firing,
		AckedCount:   acked,
		Resolved7d:   resolvedInWindow(events, 7*24*time.Hour),
		UptimePct30d: uptime,
		HasUptime:    hasUptime,
	}
}

func renderAlertsPage(w http.ResponseWriter, data AlertsPageData) error {
	tmpl, err := template.New("base.html").Funcs(funcMap).
		ParseFS(templatesFS, "templates/base.html", "templates/alerts.html")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return tmpl.ExecuteTemplate(w, "base.html", data)
}

// alertsPageHandler renders GET /alerts: viewer+ per the design doc (see
// routes.go's wiring) — every signed-in account can see alert history, but
// only an admin can ack (alertsAckHandler).
func alertsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildAlertsPageData(r, d)
		if err := renderAlertsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// ackActiveAlert mirrors one entry of serverwatch.AlertState.Active
// (anomaly.go's ActiveAlert) closely enough to both read AND write it —
// unlike handlers_dashboard.go's read-only alertStateFile, this needs
// AckedAt too (Ack's contract, anomaly.go) since this handler is the one
// producing the on-disk ack the daemon's own AlertState.MergeAckFromDisk
// later reconciles back into its in-memory copy.
type ackActiveAlert struct {
	Since   int64  `json:"since"`
	Reason  string `json:"reason"`
	Acked   bool   `json:"acked,omitempty"`
	AckedAt int64  `json:"acked_at,omitempty"`
}

// ackAlertState mirrors serverwatch.AlertState's on-disk JSON shape.
type ackAlertState struct {
	Active map[string]ackActiveAlert `json:"active"`
}

// loadAckAlertState reads path into an ackAlertState, defaulting to an
// empty (non-nil) Active map on a missing file or any decode error — same
// "never fail the page, just show/act on nothing" tolerance as
// loadActiveAlerts/loadAlertLogEvents.
func loadAckAlertState(path string) ackAlertState {
	s := ackAlertState{Active: map[string]ackActiveAlert{}}
	if path == "" {
		return s
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(b, &s)
	if s.Active == nil {
		s.Active = map[string]ackActiveAlert{}
	}
	return s
}

// saveAckAlertState writes s to path atomically (temp file + rename),
// mirroring serverwatch.AlertState.Save (anomaly.go) exactly (same
// marshal-then-atomic-rename shape, same 0o644 perm — alerts.json holds no
// secrets) so the daemon's own AlertState.Save/Load round-trip the file
// this handler writes without any format drift.
func saveAckAlertState(path string, s ackAlertState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// alertsAckHandler handles POST /alerts/{key}/ack (admin-only + CSRF — see
// routes.go's wiring): it flips the named active alert's Acked/AckedAt in
// Deps.AlertStatePath's on-disk AlertState JSON, which the running daemon
// reconciles back into its own in-memory copy via AlertState.
// MergeAckFromDisk (anomaly.go) on its next fire/recover transition — this
// handler never touches the daemon's in-process state directly (there is
// none to touch from this package; see the design note atop
// internal/serverwatch/web_deps.go for why internal/web can't import
// serverwatch to do so even if it wanted to).
func alertsAckHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := credentialFromParam(r.PathValue("key"))
		if err != nil {
			http.Error(w, "invalid alert key", http.StatusBadRequest)
			return
		}
		keyStr := string(key)

		state := loadAckAlertState(d.AlertStatePath)
		a, ok := state.Active[keyStr]
		if !ok {
			http.Error(w, fmt.Sprintf("no active alert for key %q", keyStr), http.StatusNotFound)
			return
		}
		a.Acked = true
		a.AckedAt = time.Now().Unix()
		state.Active[keyStr] = a

		if err := saveAckAlertState(d.AlertStatePath, state); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		logAudit(d, r, "alert.ack", keyStr, "acked=false", "acked=true")

		data := buildAlertsPageData(r, d)
		if err := renderAlertsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}
