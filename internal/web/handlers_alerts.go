package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"serverwatch/internal/core"
)

// alertHistoryViaAPI reads the daemon's alert-log history over the control
// socket (Deps.API.AlertHistory) rather than decoding the alertlog.jsonl file
// off disk: a plugin must not read daemon-owned state from disk. It requests
// the whole log (since 0, no limit) newest-first -- alertHistoryRecords
// (serverwatch) already sorts it that way -- and the callers cap/window it as
// before (alertHistoryRows to maxAlertHistoryRows, resolvedInWindow to 7d). A
// nil API or a read error degrades to nil (empty history) rather than failing
// the page, the same display-only tolerance the old loadAlertLogEvents had.
func alertHistoryViaAPI(d Deps) []core.AlertRecord {
	if d.API == nil {
		return nil
	}
	recs, err := d.API.AlertHistory(0, 0)
	if err != nil {
		return nil
	}
	return recs
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

// deliveredNames renders a history record's DeliveredTo (the channels that
// actually accepted the notification, OK == true, carried over the socket in
// core.AlertRecord) as a comma-joined list, or "-" if none did (or none were
// configured).
func deliveredNames(r core.AlertRecord) string {
	if len(r.DeliveredTo) == 0 {
		return "-"
	}
	return strings.Join(r.DeliveredTo, ", ")
}

// alertHistoryRows caps the log to the most recent maxAlertHistoryRows
// records (newest first, already alertHistoryViaAPI's order) for the
// "Recent history" table -- the mockup shows a bounded recent window, not
// the entire log.
const maxAlertHistoryRows = 100

func alertHistoryRows(events []core.AlertRecord) []alertHistoryRow {
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
// NOTE: this cannot be named "Active" -- PageData already declares an
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
// to now) -- the mockup's "Resolved · 7d" tile.
func resolvedInWindow(events []core.AlertRecord, window time.Duration) int {
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
// render "-" instead of a misleading 100%.
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
	active := activeAlertsViaAPI(d)
	events := alertHistoryViaAPI(d)

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
// routes.go's wiring) -- every signed-in account can see alert history, but
// only an admin can ack (alertsAckHandler).
func alertsPageHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data := buildAlertsPageData(r, d)
		if err := renderAlertsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// alertsAckHandler handles POST /alerts/{key}/ack (admin-only + CSRF -- see
// routes.go's wiring): it acks the named active alert THROUGH the control
// socket (Deps.API.AckAlert), which runs the ack daemon-side (LoadAlertState
// + Ack + Save in the daemon process) so the web plugin is not a second
// writer to the daemon's alerts.json. It first reads the current active set
// over the socket to preserve the old handler's 404 for an unknown key (a
// socket read error there is a real 500, not a masked "not found"), then
// calls AckAlert; the daemon reconciles the ack into its own in-memory
// AlertState as before.
func alertsAckHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := credentialFromParam(r.PathValue("key"))
		if err != nil {
			http.Error(w, "invalid alert key", http.StatusBadRequest)
			return
		}
		keyStr := string(key)

		if d.API == nil {
			http.Error(w, "alert control unavailable", http.StatusInternalServerError)
			return
		}

		recs, err := d.API.ActiveAlerts()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		found := false
		for _, a := range recs {
			if a.Key == keyStr {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, fmt.Sprintf("no active alert for key %q", keyStr), http.StatusNotFound)
			return
		}

		if err := d.API.AckAlert(keyStr); err != nil {
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
