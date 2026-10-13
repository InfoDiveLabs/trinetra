// internal/trinetra/statuspage_eval.go
package trinetra

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/core"
)

type statusSignal struct{ Node, Key, Severity string }

type statusInputs struct {
	Now     time.Time
	Signals []statusSignal
	Down    map[string]bool
	Maint   map[string]bool
	Tags    map[string][]string
	Known   func(t core.StatusTarget) bool
}

type stateChange struct {
	ServiceID string
	From, To  core.ServiceState
	Reason    string
}

// targetNodes returns the node ids a target covers.
func targetNodes(t core.StatusTarget, in statusInputs) []string {
	switch t.Kind {
	case core.TargetHost:
		return []string{""}
	case core.TargetTag:
		var out []string
		for id, tags := range in.Tags {
			if slices.Contains(tags, t.Value) {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out
	default: // node, container, unit, mount
		return []string{t.Node}
	}
}

// targetKey is the one alert key a narrow target watches, "" for whole-node kinds.
func targetKey(t core.StatusTarget) string {
	switch t.Kind {
	case core.TargetContainer:
		return "docker:" + t.Value
	case core.TargetUnit:
		return "service:" + t.Value
	case core.TargetMount:
		return "disk:" + t.Value
	}
	return ""
}

func describeTarget(t core.StatusTarget) string {
	node := t.Node
	if node == "" {
		node = "this host"
	}
	switch t.Kind {
	case core.TargetHost:
		return "this host"
	case core.TargetNode:
		return "node " + node
	case core.TargetTag:
		return "tag " + t.Value
	}
	return fmt.Sprintf("%s %s on %s", t.Kind, t.Value, node)
}

func nodeLabel(id string) string {
	if id == "" {
		return "this host"
	}
	return id
}

func computeServiceState(svc core.StatusService, in statusInputs) (core.ServiceState, string, []string) {
	state := core.StateOperational
	reason := ""
	var missing []string
	bump := func(s core.ServiceState, why string) {
		if core.WorseState(state, s) != state {
			state, reason = s, why
		}
	}
	for _, t := range svc.Targets {
		if in.Known != nil && !in.Known(t) {
			missing = append(missing, describeTarget(t))
			continue
		}
		key := targetKey(t)
		for _, node := range targetNodes(t, in) {
			if in.Down[node] && !in.Maint[node] { // planned downtime shows as maintenance
				bump(core.StateOutage, nodeLabel(node)+" is down")
			}
			for _, s := range in.Signals {
				if s.Node != node || (key != "" && s.Key != key) {
					continue
				}
				if s.Severity == "critical" {
					bump(core.StateOutage, fmt.Sprintf("critical alert %s on %s", s.Key, nodeLabel(node)))
				} else {
					bump(core.StateDegraded, fmt.Sprintf("warning alert %s on %s", s.Key, nodeLabel(node)))
				}
			}
			if in.Maint[node] {
				bump(core.StateMaintenance, "maintenance window on "+nodeLabel(node))
			}
		}
	}
	return state, reason, missing
}

func evaluateServices(svcs []core.StatusService, st map[string]*serviceRuntimeState, in statusInputs) ([]stateChange, []core.ServiceEvaluation) {
	var changes []stateChange
	evals := make([]core.ServiceEvaluation, 0, len(svcs))
	now := in.Now.Unix()
	for _, svc := range svcs {
		rs := st[svc.ID]
		if rs == nil {
			rs = &serviceRuntimeState{State: core.StateOperational}
			st[svc.ID] = rs
		}
		computed, reason, missing := computeServiceState(svc, in)
		switch {
		case computed == rs.State:
			rs.PendingState, rs.PendingSince = "", 0
		case computed == core.StateMaintenance || svc.HoldDownSec == 0:
			changes = append(changes, stateChange{svc.ID, rs.State, computed, reason})
			rs.State, rs.PendingState, rs.PendingSince = computed, "", 0
		case rs.PendingState != computed:
			rs.PendingState, rs.PendingSince = computed, now
		case now-rs.PendingSince >= int64(svc.HoldDownSec):
			changes = append(changes, stateChange{svc.ID, rs.State, computed, reason})
			rs.State, rs.PendingState, rs.PendingSince = computed, "", 0
		}
		if rs.History == nil {
			rs.History = map[string]core.ServiceState{}
		}
		day := dayKey(in.Now)
		rs.History[day] = core.WorseState(rs.History[day], rs.State)
		evals = append(evals, core.ServiceEvaluation{
			ServiceID: svc.ID, State: rs.State, Computed: computed,
			PendingSince: rs.PendingSince, Reason: reason, Missing: missing,
		})
	}
	// Forget runtime state of deleted services.
	for id := range st {
		if !slices.ContainsFunc(svcs, func(s core.StatusService) bool { return s.ID == id }) {
			delete(st, id)
		}
	}
	return changes, evals
}
