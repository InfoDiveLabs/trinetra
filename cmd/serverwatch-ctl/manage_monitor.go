package main

import (
	"fmt"
	"sort"
	"strconv"

	"serverwatch/internal/config"
	sw "serverwatch/internal/serverwatch"
)

// monitorTargetRow is one Monitor thresholds screen row: a discovered
// target (sw.Target, from sw.DiscoverLocal) merged with its current config
// overrides -- the same enable/threshold state `serverwatch monitor list`
// reports (systemd.go's cmdMonitor) via config.Config.TargetEnabled/
// TargetThreshold.
type monitorTargetRow struct {
	ID           string
	Kind         string
	Display      string
	Available    bool
	Enabled      bool
	Threshold    float64
	ThresholdSet bool
}

// buildMonitorRows merges targets (as sw.DiscoverLocal returns them) with
// cfg's per-target overrides into display/edit rows, sorted by ID for a
// stable on-screen order (sw.Discover's own order is grouped by kind but
// not alphabetical, and map iteration inside it is not guaranteed stable
// across kinds).
func buildMonitorRows(targets []sw.Target, cfg *config.Config) []monitorTargetRow {
	rows := make([]monitorTargetRow, 0, len(targets))
	for _, t := range targets {
		row := monitorTargetRow{
			ID: t.ID, Kind: t.Kind, Display: t.Display, Available: t.Available,
			Enabled: true,
		}
		if cfg != nil {
			row.Enabled = cfg.TargetEnabled(t.ID)
			if v, ok := cfg.TargetThreshold(t.ID); ok {
				row.Threshold = v
				row.ThresholdSet = true
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// applyMonitorEnable sets target's enabled/disabled override on cfg via the
// SAME config.Config.SetTarget setter `serverwatch monitor enable|disable`
// uses (systemd.go's cmdMonitor). SetTarget does no validation (any target
// id is accepted, matching the CLI, which never checks the id against a
// live discovery list either), so this cannot fail.
func applyMonitorEnable(cfg *config.Config, target string, enabled bool) {
	cfg.SetTarget(target, enabled)
}

// applyMonitorThreshold parses valueStr and sets target's threshold
// override on cfg via config.Config.SetTargetThreshold, the same parse +
// setter `serverwatch monitor threshold <target> <value>` uses
// (systemd.go's cmdMonitor). Returns a wrapped strconv error (nothing
// applied) when valueStr isn't a valid float.
func applyMonitorThreshold(cfg *config.Config, target, valueStr string) error {
	v, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return fmt.Errorf("threshold: %w", err)
	}
	cfg.SetTargetThreshold(target, v)
	return nil
}
