package main

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// monitorTargetRow is one Monitor thresholds screen row: a discovered
// target (core.TargetView, from api.MonitorTargets over the control
// socket) merged with its current config overrides -- the same enable/
// threshold state `trinetra monitor list` reports (systemd.go's
// cmdMonitor) via config.Config.TargetEnabled/TargetThreshold.
type monitorTargetRow struct {
	ID           string
	Kind         string
	Display      string
	Available    bool
	Enabled      bool
	Threshold    float64
	ThresholdSet bool
}

// buildMonitorRows merges targets (as api.MonitorTargets returns them) with cfg's
// per-target overrides into display/edit rows, sorted by ID for a stable on-screen order.
func buildMonitorRows(targets []core.TargetView, cfg *config.Config) []monitorTargetRow {
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

// applyMonitorEnable sets target's enabled/disabled override on cfg via the SAME
// config.Config.SetTarget setter `trinetra monitor enable|disable` uses.
func applyMonitorEnable(cfg *config.Config, target string, enabled bool) {
	cfg.SetTarget(target, enabled)
}

// applyMonitorThreshold parses valueStr and sets target's threshold override on cfg via
// config.Config.SetTargetThreshold.
func applyMonitorThreshold(cfg *config.Config, target, valueStr string) error {
	v, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return fmt.Errorf("threshold: %w", err)
	}
	cfg.SetTargetThreshold(target, v)
	return nil
}
