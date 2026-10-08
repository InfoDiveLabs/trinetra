package trinetra

import (
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/fleet"
)

// localSignals turns this host's in-memory active alerts into signals.
func localSignals(active map[string]ActiveAlert) []statusSignal {
	out := make([]statusSignal, 0, len(active))
	for key, a := range active {
		out = append(out, statusSignal{Key: key, Severity: severityString(a.Critical)})
	}
	return out
}

// gatherStandalone builds inputs for a solo host. Units are always "known":
// the snapshot only lists failed units.
func gatherStandalone(active map[string]ActiveAlert, snap Snapshot) statusInputs {
	return statusInputs{
		Signals: localSignals(active),
		Known: func(t core.StatusTarget) bool {
			if t.Node != "" || t.Kind == core.TargetNode || t.Kind == core.TargetTag {
				return false // fleet-only kinds on a solo host
			}
			switch t.Kind {
			case core.TargetContainer:
				_, ok := snap.Containers[t.Value]
				return ok
			case core.TargetMount:
				_, ok := snap.Disks[t.Value]
				return ok
			}
			return true
		},
	}
}

// maintenanceActiveForNode reports whether any maintenance window that
// could apply to the node is active now.
func maintenanceActiveForNode(s *silenceStore, now time.Time, id, name string, tags []string) bool {
	if s == nil {
		return false
	}
	for _, m := range s.Maintenances() {
		if maintenanceActiveAt(m, now) && len(applicableMatchers(m.Matchers, id, name, tags)) > 0 {
			return true
		}
	}
	return false
}

// gatherMaster builds inputs on a fleet master: its own alerts (node "")
// plus every non-revoked node's replica alerts, minus silenced/maintenance-
// suppressed ones, plus liveness and maintenance flags.
func gatherMaster(m *masterState, selfActive map[string]ActiveAlert, snap Snapshot, now time.Time) statusInputs {
	in := gatherStandalone(selfActive, snap)
	selfKnown := in.Known
	in.Signals = nil
	in.Down, in.Maint, in.Tags = map[string]bool{}, map[string]bool{}, map[string][]string{}
	keep := func(node, name string, tags []string, key, sev string) bool {
		return m.silences == nil || m.silences.Suppressed(now.Unix(), node, name, tags, key, sev) == nil
	}
	for key, a := range selfActive {
		sev := severityString(a.Critical)
		if keep("", "", nil, key, sev) {
			in.Signals = append(in.Signals, statusSignal{Key: key, Severity: sev})
		}
	}
	in.Maint[""] = maintenanceActiveForNode(m.silences, now, "", "", nil)
	known := map[string]bool{}
	for _, n := range m.reg.List() {
		if n.Revoked {
			continue
		}
		known[n.ID] = true
		in.Tags[n.ID] = n.Tags
		in.Down[n.ID] = m.tracker.State(n.ID) == fleet.StateDown
		in.Maint[n.ID] = maintenanceActiveForNode(m.silences, now, n.ID, n.Name, n.Tags)
		api, err := m.sink.NodeAPI(n.ID, m.getCfg)
		if err != nil {
			continue
		}
		recs, err := api.ActiveAlerts()
		if err != nil {
			continue
		}
		for _, rec := range recs {
			if keep(n.ID, n.Name, n.Tags, rec.Key, rec.Severity) {
				in.Signals = append(in.Signals, statusSignal{Node: n.ID, Key: rec.Key, Severity: rec.Severity})
			}
		}
	}
	in.Known = func(t core.StatusTarget) bool {
		switch {
		case t.Kind == core.TargetTag:
			return true
		case t.Node == "":
			return selfKnown(t)
		default:
			return known[t.Node] // remote container/unit/mount existence not verified (Ruling 6)
		}
	}
	return in
}
