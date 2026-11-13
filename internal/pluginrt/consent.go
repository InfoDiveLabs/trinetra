package pluginrt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

// CheckEnable validates req against rec and returns the consent to record.
func CheckEnable(rec plugin.Installed, req plugin.EnableRequest, allowUnverified bool, now int64) (plugin.ConsentRecord, error) {
	version, trust, perms := rec.Version, rec.Trust, rec.Manifest.Permissions
	if req.Pending {
		if rec.Pending == nil {
			return plugin.ConsentRecord{}, fmt.Errorf("plugin %s has no pending update", rec.ID)
		}
		version, trust, perms = rec.Pending.Version, rec.Pending.Trust, rec.Pending.Manifest.Permissions
	}
	unverified := trust != plugin.TrustVerified && trust != plugin.TrustFirstParty
	if unverified {
		if !allowUnverified {
			return plugin.ConsentRecord{}, plugin.ErrUnverifiedDisallowed
		}
		if !req.AcknowledgedRisk {
			return plugin.ConsentRecord{}, fmt.Errorf("%w: %s is unverified; the risk must be acknowledged", plugin.ErrConsentRequired, rec.ID)
		}
	}
	if missing := plugin.Missing(req.Accepted, perms); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, p := range missing {
			names[i] = string(p)
		}
		return plugin.ConsentRecord{}, fmt.Errorf("%w: %s requires %s", plugin.ErrConsentRequired, rec.ID, strings.Join(names, ", "))
	}
	granted := append([]plugin.Permission(nil), perms...)
	sort.Slice(granted, func(i, j int) bool { return granted[i] < granted[j] })
	return plugin.ConsentRecord{Time: now, Actor: req.Actor, Via: req.Via, Version: version,
		Permissions: granted, Unverified: unverified}, nil
}

// PlanUpdate returns the permissions next adds over the current grant.
func PlanUpdate(cur plugin.Installed, next plugin.Manifest) []plugin.Permission {
	return plugin.Missing(cur.Granted, next.Permissions)
}
