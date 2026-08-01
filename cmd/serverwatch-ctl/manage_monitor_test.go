package main

import (
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/core"
)

func TestBuildMonitorRowsDefaultsEnabledNoThreshold(t *testing.T) {
	targets := []core.TargetView{
		{ID: "disk:/", Kind: "disk", Display: "/", Available: true},
	}
	rows := buildMonitorRows(targets, &config.Config{})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.ID != "disk:/" || r.Kind != "disk" || r.Display != "/" || !r.Available {
		t.Errorf("row = %+v, want fields copied from the target", r)
	}
	if !r.Enabled {
		t.Error("Enabled = false, want true (default when no override)")
	}
	if r.ThresholdSet {
		t.Error("ThresholdSet = true, want false (no override)")
	}
}

func TestBuildMonitorRowsMergesOverrides(t *testing.T) {
	cfg := &config.Config{}
	cfg.SetTarget("disk:/", false)
	cfg.SetTargetThreshold("disk:/", 92.5)
	targets := []core.TargetView{
		{ID: "disk:/", Kind: "disk", Display: "/", Available: true},
	}
	rows := buildMonitorRows(targets, cfg)
	r := rows[0]
	if r.Enabled {
		t.Error("Enabled = true, want false (disabled override)")
	}
	if !r.ThresholdSet || r.Threshold != 92.5 {
		t.Errorf("Threshold = %v, ThresholdSet = %v, want 92.5, true", r.Threshold, r.ThresholdSet)
	}
}

func TestBuildMonitorRowsSortedByID(t *testing.T) {
	targets := []core.TargetView{
		{ID: "temp", Kind: "temp", Display: "cpu-thermal", Available: true},
		{ID: "disk:/", Kind: "disk", Display: "/", Available: true},
		{ID: "docker:web", Kind: "docker", Display: "web", Available: true},
	}
	rows := buildMonitorRows(targets, &config.Config{})
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	want := []string{"disk:/", "docker:web", "temp"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Errorf("rows[%d].ID = %q, want %q (want ID-sorted order %v)", i, rows[i].ID, id, want)
		}
	}
}

func TestApplyMonitorEnableSetsTargetOverride(t *testing.T) {
	cfg := &config.Config{}
	applyMonitorEnable(cfg, "disk:/", false)
	if cfg.TargetEnabled("disk:/") {
		t.Error("TargetEnabled(disk:/) = true, want false after applyMonitorEnable(..., false)")
	}
	applyMonitorEnable(cfg, "disk:/", true)
	if !cfg.TargetEnabled("disk:/") {
		t.Error("TargetEnabled(disk:/) = false, want true after applyMonitorEnable(..., true)")
	}
}

func TestApplyMonitorThresholdSetsValue(t *testing.T) {
	cfg := &config.Config{}
	if err := applyMonitorThreshold(cfg, "disk:/", "92.5"); err != nil {
		t.Fatalf("applyMonitorThreshold() error = %v, want nil", err)
	}
	v, ok := cfg.TargetThreshold("disk:/")
	if !ok || v != 92.5 {
		t.Errorf("TargetThreshold(disk:/) = %v, %v, want 92.5, true", v, ok)
	}
}

func TestApplyMonitorThresholdRejectsNonNumeric(t *testing.T) {
	cfg := &config.Config{}
	err := applyMonitorThreshold(cfg, "disk:/", "not-a-number")
	if err == nil {
		t.Fatal("applyMonitorThreshold() error = nil, want an error for a non-numeric value")
	}
	if _, ok := cfg.TargetThreshold("disk:/"); ok {
		t.Error("TargetThreshold(disk:/) set despite a rejected value")
	}
}
