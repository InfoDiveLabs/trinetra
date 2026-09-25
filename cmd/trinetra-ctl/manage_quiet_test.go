package main

import (
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestApplyQuietHoursSetsRange(t *testing.T) {
	cfg := &config.Config{}
	if err := applyQuietHours(cfg, "22-6"); err != nil {
		t.Fatalf("applyQuietHours() error = %v, want nil", err)
	}
	if cfg.QuietHours != "22-6" {
		t.Errorf("QuietHours = %q, want 22-6", cfg.QuietHours)
	}
}

func TestApplyQuietHoursOffClears(t *testing.T) {
	cfg := &config.Config{QuietHours: "22-6"}
	if err := applyQuietHours(cfg, "off"); err != nil {
		t.Fatalf("applyQuietHours() error = %v, want nil", err)
	}
	if cfg.QuietHours != "" {
		t.Errorf("QuietHours = %q, want cleared", cfg.QuietHours)
	}
}

func TestApplyQuietHoursInvalidRejected(t *testing.T) {
	cfg := &config.Config{}
	err := applyQuietHours(cfg, "not-a-range")
	if err == nil {
		t.Fatal("applyQuietHours() error = nil, want a validation error")
	}
	if cfg.QuietHours != "" {
		t.Errorf("QuietHours = %q, want left unset on a rejected value", cfg.QuietHours)
	}
}
