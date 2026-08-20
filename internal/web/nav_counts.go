package web

import "strconv"

// NavCounts holds the small per-request counts rendered as the sidebar nav's
// badges (base.html's "nav" block, NavItem.Badge): Alerts/Channels/Users/
// Monitoring. These replace the mockup's hardcoded demo values (220/2/5/3) --
// see navCountsFor's doc for exactly what each counts and how it degrades.
type NavCounts struct {
	// Alerts is the number of entries in the daemon's current active-alert
	// set (Deps.API.ActiveAlerts, over the control socket), i.e. how many
	// conditions are firing right
	// now (regardless of ack state -- ack only changes notification/status
	// pill behavior elsewhere, not whether the condition itself is active).
	Alerts int
	// Channels is len(Deps.Cfg().Channels): how many notification channels
	// are configured.
	Channels int
	// Users is the number of accounts in the web user store
	// (<StateDir>/users.json).
	Users int
	// Monitoring is the live snapshot's container count
	// (DashboardView.ContainersTotal) -- the brief's "stable default" choice
	// for a single meaningful number on the Monitoring nav entry.
	Monitoring int
}

// navCountsFor computes NavCounts from Deps: a handful of cheap, per-request
// reads (one small JSON decode, one config field, one JSON-file user-store
// list, one already-computed snapshot field) -- safe to call on every page
// render. Every source is defensive: a nil Cfg/Snapshot func, a missing or
// socket read error, or an empty/absent user store all degrade to 0
// rather than panicking or failing the page, mirroring
// activeAlertsViaAPI/buildDashboardPageData's existing tolerance for the same
// inputs.
func navCountsFor(d Deps) NavCounts {
	var c NavCounts

	c.Alerts = len(activeAlertsViaAPI(d))

	if d.Cfg != nil {
		if cfg := d.Cfg(); cfg != nil {
			c.Channels = len(cfg.Channels)
		}
	}

	c.Users = len(newUserStore(d.StateDir).List())

	if d.Snapshot != nil {
		c.Monitoring = d.Snapshot().ContainersTotal
	}

	return c
}

// badgeText renders a count as a nav badge string: empty for zero (or
// negative/unknown) so base.html's `{{if .Badge}}` guard hides the badge
// span entirely rather than showing a stale-looking literal "0".
func badgeText(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}
