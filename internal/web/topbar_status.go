package web

import "fmt"

// topbarStatus derives the shared topbar status pill (color class + display
// text) from the real active-alert set (Deps.API.ActiveAlerts, the same read
// navCountsFor uses for the sidebar's Alerts badge -- see activeAlertsViaAPI).
// It replaces the old hardcoded statusText mapping, which said "2 alerts
// firing" whenever the caller-supplied status was "crit" and "1 warning"
// whenever it was "warn", no matter how many alerts were actually active.
//
// Severity comes from each active alert's Critical flag
// (trinetra.ActiveAlert.Critical, set at fire time from the breaching
// Check's own Critical field): any active critical alert makes the pill
// "crit"; short of that, any active (non-critical) alert makes it "warn";
// with none active it's "ok". Ack state is deliberately NOT weighed here --
// an acked alert is still an active condition, just a silenced notification
// (see AlertState.Ack's doc, internal/trinetra/anomaly.go) -- the /alerts
// page's own firing/acked breakdown is a separate, more detailed view.
//
// The crit branch's count is the TOTAL active-alert count (B4, 2026-09-25 UI
// audit), not just the critical ones: /alerts' "N firing now", the
// dashboard's active-alerts panel and the sidebar's Alerts badge
// (NavCounts.Alerts) all count len(alerts) regardless of severity, so the
// pill must report the same number rather than silently dropping the
// warnings whenever a critical alert is also active.
func topbarStatus(alerts []activeAlertView) (status, text string) {
	var crit, warn int
	for _, a := range alerts {
		if a.Critical {
			crit++
		} else {
			warn++
		}
	}
	switch {
	case crit > 0:
		n := crit + warn
		return "crit", fmt.Sprintf("%d alert%s firing", n, plural(n))
	case warn > 0:
		return "warn", fmt.Sprintf("%d warning%s", warn, plural(warn))
	default:
		return "ok", "All systems normal"
	}
}

// plural returns "s" for any count but exactly 1, for topbarStatus's
// alert/warning counts ("1 alert firing" vs "2 alerts firing").
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
