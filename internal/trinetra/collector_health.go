// Package trinetra: collector_health.go makes slow-tier collection fail-visible (#110).
package trinetra

import "fmt"

// collectorAlertThreshold is how many CONSECUTIVE failed/timed-out cycles a slow-tier
// collector must accumulate before it raises a `collector:<name>` alert.
const collectorAlertThreshold = 3

// slowCollectorKeys is the fixed set of slow-tier collectors whose health is tracked.
var slowCollectorKeys = []string{"disk", "docker", "services", "smart"}

// CollectorStat is one collector's rolling health (#110): Fails is the count of consecutive
// failed cycles (0 when healthy).
type CollectorStat struct {
	Fails           int    `json:"fails"`
	LastSuccessUnix int64  `json:"last_success_unix"`
	LastError       string `json:"last_error,omitempty"`
}

// recordCollectorErr notes that a slow-tier collector was attempted this cycle but failed,
// storing its error text under CollectorErrors[key] (#110).
func recordCollectorErr(s *Snapshot, key string, err error) {
	if s.CollectorErrors == nil {
		s.CollectorErrors = map[string]string{}
	}
	s.CollectorErrors[key] = err.Error()
}

// collectorHealth accumulates per-collector CollectorStat across slow cycles.
type collectorHealth struct {
	stats map[string]CollectorStat
}

func newCollectorHealth() *collectorHealth {
	return &collectorHealth{stats: map[string]CollectorStat{}}
}

// observe folds one cycle's outcome into the health map: for every attempted collector, a
// success resets its failure streak and stamps LastSuccessUnix; a failure.
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

// carryForwardFailedCollectors copies prev's slow-tier fields into cur for every collector
// that errored this cycle (present in cur.CollectorErrors).
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

// buildCollectorChecks turns per-collector health into alert Checks (#110).
func buildCollectorChecks(snap Snapshot, interval int) []Check {
	var checks []Check
	for _, key := range slowCollectorKeys {
		st, ok := snap.CollectorHealth[key]
		if !ok {
			continue // never attempted yet: nothing to assert
		}
		// A threshold check firing exactly when Fails >= collectorAlertThreshold (Check.breach:
		// HasThreshold && Value >= Threshold).
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
