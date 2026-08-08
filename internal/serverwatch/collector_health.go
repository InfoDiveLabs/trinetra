// Package serverwatch: collector_health.go makes slow-tier collection
// fail-visible (#110). A monitoring daemon must never silently degrade: when a
// collection command (docker/df/systemctl/smartctl) fails or times out, that is
// itself a monitoring failure the operator should be alerted to, not a reason
// to publish missing data or flip a healthy target to "gone".
//
// Two mechanisms live here:
//   - carryForwardFailedCollectors preserves the previous cycle's values for any
//     collector that errored this cycle, so a single failed `docker ps` does not
//     blank the container set (making every container look gone).
//   - collectorHealth tracks per-collector consecutive failures / last success /
//     last error across cycles, so a sustained failure becomes a
//     `collector:<name>` alert (see buildCollectorChecks) and is observable in
//     status.json, and recovers automatically on the next success.
package serverwatch

import "fmt"

// collectorAlertThreshold is how many CONSECUTIVE failed/timed-out cycles a
// slow-tier collector must accumulate before it raises a `collector:<name>`
// alert (#110). 1-2 transient blips (a busy docker daemon, a slow smartctl)
// carry last-known forward silently; a persistent failure means monitoring
// itself is degraded and the operator should know.
const collectorAlertThreshold = 3

// slowCollectorKeys is the fixed set of slow-tier collectors whose health is
// tracked. Kept explicit (rather than derived from whatever errored) so a
// collector that has never failed still reports a healthy CollectorStat once
// it has succeeded, and so the alert set is stable.
var slowCollectorKeys = []string{"disk", "docker", "services", "smart"}

// CollectorStat is one collector's rolling health (#110): Fails is the count of
// consecutive failed cycles (0 when healthy), LastSuccessUnix is the unix time
// of its last successful collection (0 if never), and LastError is the most
// recent error text (cleared on success).
type CollectorStat struct {
	Fails           int    `json:"fails"`
	LastSuccessUnix int64  `json:"last_success_unix"`
	LastError       string `json:"last_error,omitempty"`
}

// recordCollectorErr notes that a slow-tier collector was attempted this cycle
// but failed, storing its error text under CollectorErrors[key] (#110). The
// slow-collector goroutine reads these to carry last-known values forward and
// to update CollectorHealth.
func recordCollectorErr(s *Snapshot, key string, err error) {
	if s.CollectorErrors == nil {
		s.CollectorErrors = map[string]string{}
	}
	s.CollectorErrors[key] = err.Error()
}

// collectorHealth accumulates per-collector CollectorStat across slow cycles.
// It is owned exclusively by the slow-collector goroutine (like netRate/
// procCPU/smartState), so it needs no locking; a value copy is published on
// each snapshot for the sampler/web to read.
type collectorHealth struct {
	stats map[string]CollectorStat
}

func newCollectorHealth() *collectorHealth {
	return &collectorHealth{stats: map[string]CollectorStat{}}
}

// observe folds one cycle's outcome into the health map: for every attempted
// collector, a success resets its failure streak and stamps LastSuccessUnix; a
// failure (an entry in errs) increments the streak and records the error. A
// collector NOT attempted this cycle (disabled, or not applicable) is left
// untouched so its last-known health persists rather than decaying.
//
// attempted is the set of collectors that actually ran this cycle; errs maps a
// collector to its error text when it failed. now is the cycle's unix time.
func (h *collectorHealth) observe(attempted map[string]bool, errs map[string]string, now int64) {
	for key := range attempted {
		st := h.stats[key]
		if msg, failed := errs[key]; failed {
			st.Fails++
			st.LastError = msg
		} else {
			st.Fails = 0
			st.LastError = ""
			st.LastSuccessUnix = now
		}
		h.stats[key] = st
	}
}

// snapshot returns a value copy of the current health map for publishing on a
// Snapshot (so the sampler/web read a stable copy, never the live map).
func (h *collectorHealth) snapshot() map[string]CollectorStat {
	if len(h.stats) == 0 {
		return nil
	}
	out := make(map[string]CollectorStat, len(h.stats))
	for k, v := range h.stats {
		out[k] = v
	}
	return out
}

// carryForwardFailedCollectors copies prev's slow-tier fields into cur for every
// collector that errored this cycle (present in cur.CollectorErrors), so a
// transient collection failure preserves the last-known values instead of
// publishing a snapshot with a healthy target flipped to gone/empty (#110).
// cur is marked SlowStale because at least one field is carried-forward rather
// than freshly collected. prev is the previous published slow Snapshot.
func carryForwardFailedCollectors(prev *Snapshot, cur *Snapshot) {
	if len(cur.CollectorErrors) == 0 || prev == nil {
		return
	}
	if _, failed := cur.CollectorErrors["disk"]; failed {
		cur.Disks = prev.Disks
		cur.DiskDetail = prev.DiskDetail
	}
	if _, failed := cur.CollectorErrors["docker"]; failed {
		cur.Containers = prev.Containers
		cur.ContainerStats = prev.ContainerStats
	}
	if _, failed := cur.CollectorErrors["services"]; failed {
		cur.FailedUnits = prev.FailedUnits
		cur.Units = prev.Units
	}
	if _, failed := cur.CollectorErrors["smart"]; failed {
		cur.SmartHealth = prev.SmartHealth
		cur.SmartAttrs = prev.SmartAttrs
	}
	cur.SlowStale = true
}

// buildCollectorChecks turns per-collector health into alert Checks (#110): a
// collector whose consecutive-failure count has reached collectorAlertThreshold
// fires `collector:<name>`, with a message naming the last error. A collector
// below the threshold (or recovered) produces a non-firing check so the alert
// recovers automatically on the next success (alerts.Evaluate recovers any
// active alert whose check no longer breaches).
func buildCollectorChecks(snap Snapshot, interval int) []Check {
	var checks []Check
	for _, key := range slowCollectorKeys {
		st, ok := snap.CollectorHealth[key]
		if !ok {
			continue // never attempted yet: nothing to assert
		}
		// A threshold check firing exactly when Fails >= collectorAlertThreshold
		// (Check.breach: HasThreshold && Value >= Threshold). Below the
		// threshold Value < Threshold so it does not fire, and alerts.Evaluate
		// recovers any active collector alert the moment a success drops Fails
		// back to 0. FireMsg is consulted only when firing.
		checks = append(checks, Check{
			Key:          "collector:" + key,
			HasThreshold: true,
			Threshold:    float64(collectorAlertThreshold),
			Value:        float64(st.Fails),
			Critical:     true,
			Interval:     interval,
			FireMsg:      collectorFailMsg(key, st),
		})
	}
	return checks
}

// collectorFailMsg is the alert fire message for a failing collector, naming
// the collector, its consecutive-failure count, and the last error seen.
func collectorFailMsg(key string, st CollectorStat) string {
	msg := fmt.Sprintf("collector %s failing: %d consecutive collection failures", key, st.Fails)
	if st.LastError != "" {
		msg += " (last error: " + st.LastError + ")"
	}
	return msg
}
