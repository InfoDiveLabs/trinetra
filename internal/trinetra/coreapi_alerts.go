// Package trinetra: coreapi_alerts.go holds the AlertState/AlertLog ->
// []core.AlertRecord mapping shared by both core.API implementations
// (inprocAPI.ActiveAlerts/AlertHistory in coreapi_inproc.go, and
// fileAPI.ActiveAlerts/AlertHistory in coreapi_file.go). Before this file
// existed, both methods carried an independent ~20-line copy of this
// mapping; factoring it out here means a future field change (or bug fix)
// can't update one implementation and silently miss the other -- there is
// now exactly one place the AlertRecord shape gets built from an
// AlertState/AlertLog.
//
// Deliberately UNTAGGED, same reasoning as coreapi_inproc.go/coreapi_file.go:
// this only touches internal/core's stdlib+config-only DTOs and this
// package's own AlertState/AlertLog types, so it never pulls internal/web's
// third-party dependencies into the default build.
package trinetra

import (
	"sort"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// severityString renders an ActiveAlert.Critical bool as the same "critical"/"warning"
// strings Severity.String() (notifier.go) produces.
func severityString(critical bool) string {
	if critical {
		return SevCritical.String()
	}
	return SevWarning.String()
}

// activeAlertRecords maps state.Active into []core.AlertRecord: Kind is left "" (an active
// alert has no fire/recover distinction of its own -- see core.AlertRecord's doc).
func activeAlertRecords(state *AlertState) []core.AlertRecord {
	keys := make([]string, 0, len(state.Active))
	for k := range state.Active {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]core.AlertRecord, 0, len(keys))
	for _, k := range keys {
		aa := state.Active[k]
		out = append(out, core.AlertRecord{
			Key:      k,
			Severity: severityString(aa.Critical),
			Kind:     "",
			Source:   aa.Reason,
			Time:     aa.Since,
			Acked:    aa.Acked,
			AckedAt:  aa.AckedAt,
		})
	}
	return out
}

// anyDelivered reports whether at least one of an AlertEvent's Delivery records actually
// reached its channel (OK true); false when every attempt failed or none was recorded.
func anyDelivered(ds []Delivery) bool {
	for _, d := range ds {
		if d.OK {
			return true
		}
	}
	return false
}

// deliveredChannels returns the names of the channels that actually accepted delivery
// (Delivery.OK true), in record order -- the per-channel detail behind anyDelivered's bool.
func deliveredChannels(ds []Delivery) []string {
	var out []string
	for _, d := range ds {
		if d.OK {
			out = append(out, d.Channel)
		}
	}
	return out
}

// alertHistoryRecords loads log's events since sinceUnix and maps each into
// a core.AlertRecord (a direct field-for-field mapping -- AlertEvent already
// carries Key/Severity/Kind/Source/Time/Title). Acked and AckedAt are always
// false/0: the alert log is a history of past fire/recover dispatches, not
// the current ack state (that's ActiveAlerts' job) -- AlertEvent carries no
// ack timestamp of its own. Delivered is derived via anyDelivered: true when
// at least one of the event's Delivery records actually reached its
// channel. Results are newest-first and capped to limit (limit<=0 means
// unbounded), mirroring the web alerts page's "Recent history" table
// (internal/web/handlers_alerts.go's alertHistoryRows). Shared by
// inprocAPI.AlertHistory and fileAPI.AlertHistory -- both just call
// NewAlertLog against their own alertLogPath() and hand the result here.
func alertHistoryRecords(log *AlertLog, sinceUnix int64, limit int) ([]core.AlertRecord, error) {
	evs, err := log.AlertEventsSince(sinceUnix)
	if err != nil {
		return nil, err
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Time > evs[j].Time })
	if limit > 0 && len(evs) > limit {
		evs = evs[:limit]
	}

	out := make([]core.AlertRecord, 0, len(evs))
	for _, ev := range evs {
		out = append(out, core.AlertRecord{
			Key:         ev.Key,
			Severity:    ev.Severity,
			Kind:        ev.Kind,
			Source:      ev.Source,
			Time:        ev.Time,
			Acked:       false,
			Title:       ev.Title,
			Delivered:   anyDelivered(ev.Delivered),
			DeliveredTo: deliveredChannels(ev.Delivered),
		})
	}
	return out, nil
}
