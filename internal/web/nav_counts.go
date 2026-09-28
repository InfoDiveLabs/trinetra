package web

import (
	"net/http"
	"strconv"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

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
	// FleetDown (task 5, fleet-web-a) is how many fleet nodes are currently
	// State=="down", for the "Fleet" nav entry's badge -- 0 (no badge) on
	// every page where this daemon isn't a fleet master, since
	// navCountsFor only computes it when fleetRole=="master" (see its doc).
	FleetDown int
	// IncidentsFiring (task C2, fleet incidents web UI) is how many fleet
	// incidents are currently State=="firing", for the "Incidents" nav
	// entry's badge -- 0 (no badge) whenever this daemon isn't a fleet
	// master, exactly like FleetDown. Read through the request-scoped
	// fleetMemo's fleetIncidentsFiringCount (fleet_memo.go), so this costs a
	// real round trip only once per request.
	IncidentsFiring int
}

// navCountsFor computes NavCounts from Deps: a handful of cheap, per-request
// reads (one small JSON decode, one config field, one JSON-file user-store
// list, one already-computed snapshot field) -- safe to call on every page
// render. Every source is defensive: a nil Cfg/Snapshot func, a missing or
// socket read error, or an empty/absent user store all degrade to 0
// rather than panicking or failing the page, mirroring
// activeAlertsViaAPI/buildDashboardPageData's existing tolerance for the same
// inputs.
//
// Alerts and Monitoring reflect the request's node scope (node_scope.go):
// they're daemon/core.API concepts, so a /n/{node}/... page's badges show
// that node's own counts. Channels and Users stay the master's own values
// regardless of scope -- they're master-local concepts (config channels,
// this trinetra-web instance's own account store), never node-scoped
// (global-constraints.md).
//
// fleetRole is the caller's already-resolved fleetRole (newPageData's
// resolveFleetPageInfo, computed once per request); FleetDown is skipped
// outright (stays 0, no badge) on every solo/child/non-fleet request. When
// it does run, it reads r's request-scoped fleetMemo (fleet_memo.go)
// instead of calling Fleet().Nodes() directly -- the roster is very likely
// already cached from resolveMasterAndNodes/fetchFleetNodes/fleetRole
// itself having asked for it earlier in this same request, so this costs a
// real round trip only when nothing else in the request already paid for
// one.
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
	} else if api := apiFor(r, d); api != nil {
		if v, err := api.Snapshot(); err == nil {
			c.Monitoring = v.ContainersTotal
		}
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

// badgeText renders a count as a nav badge string: empty for zero (or
// negative/unknown) so base.html's `{{if .Badge}}` guard hides the badge
// span entirely rather than showing a stale-looking literal "0".
func badgeText(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}
