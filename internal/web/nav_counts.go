package web

import (
	"net/http"
	"strconv"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// NavCounts holds the small per-request counts rendered as the sidebar nav's badges
// (base.html's "nav" block, NavItem.Badge): Alerts/Channels/Users/ Monitoring.
type NavCounts struct {
	// Alerts is the number of entries in the daemon's current active-alert set
	// (Deps.API.ActiveAlerts, over the control socket).
	Alerts int
	// Channels is len(Deps.Cfg().Channels): how many notification channels are configured.
	Channels int
	// Users is the number of accounts in the web user store (<StateDir>/users.json).
	Users int
	// Monitoring is the live snapshot's container count (DashboardView.ContainersTotal) -- the
	// "stable default" choice for a single meaningful number on the Monitoring nav entry.
	Monitoring int
	// FleetDown is how many fleet nodes are currently State=="down", for the "Fleet" nav
	// entry's badge -- 0 (no badge) on every page where this daemon isn't a fleet master.
	FleetDown int
	// IncidentsFiring is how many fleet incidents are currently State=="firing", for the
	// "Incidents" nav entry's badge -- 0 (no badge) whenever this daemon isn't a fleet master.
	IncidentsFiring int
}

// navCountsFor computes NavCounts from Deps: a handful of cheap, per-request reads.
func navCountsFor(r *http.Request, d Deps, fleetRole string) NavCounts {
	var c NavCounts

	c.Alerts = len(activeAlertsViaAPI(r, d))

	if d.Cfg != nil {
		if cfg := d.Cfg(); cfg != nil {
			c.Channels = len(cfg.Channels)
		}
	}

	c.Users = len(newUserStore(d.StateDir).List())

	if nodeFrom(r).Self {
		if d.Snapshot != nil {
			c.Monitoring = d.Snapshot().ContainersTotal
		}
	} else if v, err := snapshotViaAPI(r, d); err == nil {
		// snapshotViaAPI is memoized per request, shared with buildDashboardPageData's own
		// Snapshot read.
		c.Monitoring = v.ContainersTotal
	}

	if fleetRole == config.RoleMaster {
		if nodes, err := fleetMemoFrom(r).fleetNodes(d); err == nil {
			for _, n := range nodes {
				if n.State == "down" {
					c.FleetDown++
				}
			}
		}
		if n, err := fleetMemoFrom(r).fleetIncidentsFiringCount(d); err == nil {
			c.IncidentsFiring = n
		}
	}

	return c
}

// badgeText renders a count as a nav badge string: empty for zero.
func badgeText(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}
