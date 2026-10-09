// Package trinetra: swarm.go collapses Docker Swarm task containers to their
// SERVICE (#118). A Swarm task container is named "<service>.<slot>.<taskid>"
// where taskid changes on every (re)deploy, so keying series/alerts/UI on the
// raw name means every rolling deploy (a) creates two new permanent series
// (docker:<task>:cpu/mem), the root of the 3035-file cardinality explosion
// (#112/#109), and (b) fires false "container down" churn as old task names
// disappear. Keyed on the stable service name instead, cardinality tracks the
// service count and a rolling deploy no longer flaps.
//
// This is gated on Swarm actually being active (dockerAccess.swarm, probed via
// `docker info`), so a plain-docker host is completely unaffected: swarmService
// only matches the strict task-name shape, and collapseSwarmTasks is only
// called when Swarm is detected.
package trinetra

import (
	"regexp"
	"sort"
)

// swarmTaskRe matches a Swarm task container name "<service>.<slot>.<taskid>": - service:
// any non-empty prefix (may contain dashes/underscores/dots) - slot: the replica number.
var swarmTaskRe = regexp.MustCompile(`^(.+)\.([a-z0-9]+)\.([a-z0-9]{20,})$`)

// swarmServiceName returns the service name for a Swarm task container name, or
// ("", false) when name is not a Swarm task (a plain container, kept as-is).
func swarmServiceName(name string) (string, bool) {
	m := swarmTaskRe.FindStringSubmatch(name)
	if m == nil || m[1] == "" {
		return "", false
	}
	return m[1], true
}

// serviceKey maps a container name to the key it should be tracked under: the
// Swarm service name when it is a task, else the name unchanged.
func serviceKey(name string) string {
	if svc, ok := swarmServiceName(name); ok {
		return svc
	}
	return name
}

// collapseSwarmTasks rewrites the per-task container state and stats maps to be keyed by
// SERVICE (#118).
func collapseSwarmTasks(containers map[string]string, stats map[string]ContainerStat) (map[string]string, map[string]ContainerStat) {
	var outStates map[string]string
	if containers != nil {
		// Gather each service's task states, then reduce to one.
		byService := map[string][]string{}
		for name, state := range containers {
			byService[serviceKey(name)] = append(byService[serviceKey(name)], state)
		}
		outStates = make(map[string]string, len(byService))
		for svc, states := range byService {
			outStates[svc] = reduceServiceState(states)
		}
	}

	var outStats map[string]ContainerStat
	if stats != nil {
		outStats = make(map[string]ContainerStat, len(stats))
		for name, s := range stats {
			key := serviceKey(name)
			agg := outStats[key]
			agg.Name = key
			agg.CPUPct += s.CPUPct
			agg.MemMiB += s.MemMiB
			agg.NetRxMB += s.NetRxMB
			agg.NetTxMB += s.NetTxMB
			outStats[key] = agg
		}
	}
	return outStates, outStats
}

// reduceServiceState collapses a service's per-task states into one: "running" if any task
// is running, otherwise a deterministic representative of the non-running states.
func reduceServiceState(states []string) string {
	for _, st := range states {
		if st == "running" {
			return "running"
		}
	}
	sorted := append([]string(nil), states...)
	sort.Strings(sorted)
	if len(sorted) == 0 {
		return ""
	}
	return sorted[0]
}
