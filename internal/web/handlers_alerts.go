package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// alertHistoryViaAPI reads the daemon's alert-log history over the control
// socket (Deps.API.AlertHistory) rather than decoding the alertlog.jsonl file
// off disk: a plugin must not read daemon-owned state from disk. It requests
// the whole log (since 0, no limit) newest-first -- alertHistoryRecords
// (trinetra) already sorts it that way -- and the callers cap/window it as
// before (alertHistoryRows to maxAlertHistoryRows, resolvedInWindow to 7d). A
// nil API or a read error degrades to nil (empty history) rather than failing
// the page, the same display-only tolerance the old loadAlertLogEvents had.
// Reads through apiFor(r, d) (node_scope.go), so a request scoped to a fleet
// node sees that node's own alert history rather than the master's.
func alertHistoryViaAPI(r *http.Request, d Deps) []core.AlertRecord {
	api := apiFor(r, d)
	if api == nil {
		return nil
	}
	recs, err := api.AlertHistory(0, 0)
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
			Severity: ev.Severity,
			Title:    ev.Title,
			Source:   ev.Source,
			Kind:     ev.Kind,
			// When routes through silenceTimeText (handlers_fleet_silences.go,
			// master-local zone with abbreviation) rather than its own
			// unlabeled-UTC format -- round-2 review finding I1, matching
			// incidentTimeText's own fix (handlers_fleet.go) so every absolute
			// timestamp on the fleet surface uses the one convention.
			When:      silenceTimeText(ev.Time),
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
	// NodeRemote is true when this page is scoped to a non-self fleet node
	// (node_scope.go's nodeFrom(r).Self == false).
	NodeRemote bool
	// NodeConnected reports whether ack/unack should render as LIVE actions:
	// always true for the self scope, and (task C6 ruling) true for a
	// remote scope only when the roster's own NodeSummary.State (as of the
	// request-scoped fleetMemo, node_scope.go) is "online" -- the same
	// state string fleet_provider.go/liveness.go's fleet.StateOnline
	// reports for a node whose stream is currently connected. When false,
	// templates/alerts.html renders every Ack/Unack control disabled with
	// RemoteReason instead of a live POST form.
	NodeConnected bool
	// RemoteReason is the fixed "node is not connected" text
	// (nodeNotConnectedReason, handlers_logs.go) templates/alerts.html
	// renders as the disabled Ack/Unack button's label/title when
	// NodeRemote && !NodeConnected -- carried as data instead of a second
	// hardcoded literal in the template.
	RemoteReason string
	// Flash/FlashErr surface a fixed-code redirect flash (resolveAlertsFlash
	// below) for a remote ack/unack's backend failure -- the ONLY case this
	// page ever redirects rather than re-rendering directly, since a remote
	// ack/unack is a fire-and-forget push over the fleet stream with no rich
	// per-field validation to show inline (task C6 ruling).
	Flash    string
	FlashErr bool
}

// nodeConnectedFor reports whether ack/unack should be treated as a live
// action for r's node scope: always true for self, and for a remote scope,
// true only when its roster NodeSummary.State is "online" (see
// AlertsPageData.NodeConnected's doc for why this exact string).
func nodeConnectedFor(r *http.Request) bool {
	ns := nodeFrom(r)
	return ns.Self || ns.Summary.State == "online"
}

// resolveAlertsFlash resolves GET /alerts' (or a node-scoped .../alerts')
// ?flash= into display text, from a FIXED set of codes only -- exactly like
// resolveManagedFlash/resolveIncidentFlash: never render whatever ?flash=
// literally carries as free text, since it's attacker-controlled on a GET.
// Both codes here name a remote ack/unack failure (alertsAckOrUnackHandler
// below); there is no success code -- a successful remote ack/unack just
// redirects back to the same page with no flash at all, and the page's own
// re-rendered state (the alert no longer firing/now acked) is the feedback.
func resolveAlertsFlash(r *http.Request) (text string, isErr bool) {
	switch r.URL.Query().Get("flash") {
	case "remote-offline":
		return "This node is not connected -- the action could not be sent.", true
	case "remote-failed":
		return "The action could not be completed on this node.", true
	}
	return "", false
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

// uptimePct30d computes the mockup's "Uptime · 30d" tile: 100% minus the
// fraction of the last 30 days spent in a downtime event. For the self
// scope it reads Deps.Events (the same downtime event store the history
// page's panel uses, events_store.go) -- the cheap, already-wired local
// path. For a request scoped to a remote fleet node (node_scope.go), that
// local store only ever holds the master's own downtime, so it instead
// reads through apiFor(r, d).Events, the node's own event log over the
// control socket. Returns (0, false) when there's nothing to read (a nil
// Deps.Events on the self scope, a nil API on a remote scope, or a read
// error) so the caller can render "-" instead of a misleading 100%.
func uptimePct30d(r *http.Request, d Deps) (float64, bool) {
	const window = 30 * 24 * time.Hour
	to := time.Now().Unix()
	from := time.Now().Add(-window).Unix()

	var evs []core.DownEventView
	var err error
	if nodeFrom(r).Self {
		if d.Events == nil {
			return 0, false
		}
		evs, err = d.Events.Events(from, to)
	} else {
		api := apiFor(r, d)
		if api == nil {
			return 0, false
		}
		evs, err = api.Events(from, to)
	}
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
	active := activeAlertsViaAPI(r, d)
	events := alertHistoryViaAPI(r, d)

	firing, acked := 0, 0
	for _, a := range active {
		if a.Acked {
			acked++
		} else {
			firing++
		}
	}
	uptime, hasUptime := uptimePct30d(r, d)

	flash, flashErr := resolveAlertsFlash(r)
	return AlertsPageData{
		PageData:      newPageData(r, d, "Alerts & incidents", "Firing now + history"),
		ActiveAlerts:  activeAlertRows(active),
		History:       alertHistoryRows(events),
		FiringCount:   firing,
		AckedCount:    acked,
		Resolved7d:    resolvedInWindow(events, 7*24*time.Hour),
		UptimePct30d:  uptime,
		HasUptime:     hasUptime,
		NodeRemote:    !nodeFrom(r).Self,
		NodeConnected: nodeConnectedFor(r),
		RemoteReason:  nodeNotConnectedReason,
		Flash:         flash,
		FlashErr:      flashErr,
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

// alertsRedirectHref returns the href GET /alerts (or, for a node-scoped
// request, GET /n/{id}/alerts) redirects back to, with an optional ?flash=
// code appended -- shared by the remote branch of alertsAckOrUnackHandler
// below.
func alertsRedirectHref(r *http.Request, flashCode string) string {
	href := nodeHref(nodeFrom(r).Prefix, "/alerts")
	if flashCode != "" {
		href += "?flash=" + url.QueryEscape(flashCode)
	}
	return href
}

// alertsAckOrUnackHandler builds POST /alerts/{key}/ack|unack's handler
// (admin-only + CSRF -- see routes.go's wiring): it acks/unacks the named
// active alert through core.API (apiFor(r,d)), first reading the current
// active set to preserve the old handler's 404 for an unknown key (a read
// error there is a real 500, not a masked "not found").
//
// The two scopes behave differently on from here (task C6 ruling):
//
//   - Self scope (the pre-existing behavior, byte-for-byte unchanged for
//     ack): AckAlert/UnackAlert runs against THIS daemon's own in-memory
//     AlertState (LoadAlertState + Ack/Unack + Save, daemon-side), which
//     can't itself fail for a key ActiveAlerts() just confirmed exists, so
//     any error here is a genuine 500. On success the page re-renders
//     in-place at 200 -- no redirect, no flash.
//   - Remote scope: AckAlert/UnackAlert instead pushes an ack/unack frame
//     down the node's stream connection (replicaAPI.remoteAck,
//     fleet_replica.go) and can fail with exactly "node is not connected"
//     (the button is disabled client-side when the roster reports the node
//     offline, but a race -- the node dropping between page load and this
//     POST -- can still reach here). Because this is a fire-and-forget push
//     with no rich per-field error to show inline, a failure redirects back
//     to the node-scoped /alerts page with a FIXED flash code
//     (resolveAlertsFlash) rather than rendering free-form error text.
//     Success also redirects (so the page re-reads the roster's current
//     Node scope/state), with no flash -- the re-rendered alert list is
//     the feedback.
func alertsAckOrUnackHandler(d Deps, unack bool) http.HandlerFunc {
	auditAction := "alert.ack"
	if unack {
		auditAction = "alert.unack"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		key, err := credentialFromParam(r.PathValue("key"))
		if err != nil {
			http.Error(w, "invalid alert key", http.StatusBadRequest)
			return
		}
		keyStr := string(key)

		api := apiFor(r, d)
		if api == nil {
			http.Error(w, "alert control unavailable", http.StatusInternalServerError)
			return
		}

		recs, err := api.ActiveAlerts()
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

		remote := !nodeFrom(r).Self
		if unack {
			err = api.UnackAlert(keyStr)
		} else {
			err = api.AckAlert(keyStr)
		}
		if err != nil {
			if remote {
				flashCode := "remote-failed"
				if err.Error() == nodeNotConnectedReason {
					flashCode = "remote-offline"
				}
				http.Redirect(w, r, alertsRedirectHref(r, flashCode), http.StatusSeeOther)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		oldVal, newVal := "acked=false", "acked=true"
		if unack {
			oldVal, newVal = "acked=true", "acked=false"
		}
		logAudit(d, r, auditAction, keyStr, oldVal, newVal)

		if remote {
			http.Redirect(w, r, alertsRedirectHref(r, ""), http.StatusSeeOther)
			return
		}
		data := buildAlertsPageData(r, d)
		if err := renderAlertsPage(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

// alertsAckHandler handles POST /alerts/{key}/ack -- see
// alertsAckOrUnackHandler's doc for the full contract.
func alertsAckHandler(d Deps) http.HandlerFunc { return alertsAckOrUnackHandler(d, false) }

// alertsUnackHandler handles POST /alerts/{key}/unack -- see
// alertsAckOrUnackHandler's doc for the full contract.
func alertsUnackHandler(d Deps) http.HandlerFunc { return alertsAckOrUnackHandler(d, true) }
