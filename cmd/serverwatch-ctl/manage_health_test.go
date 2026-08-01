package main

import (
	"testing"

	"serverwatch/internal/config"
)

func TestApplyHealthchecksSetsURL(t *testing.T) {
	cfg := &config.Config{}
	if err := applyHealthchecks(cfg, "https://hc-ping.com/abc"); err != nil {
		t.Fatalf("applyHealthchecks() error = %v, want nil", err)
	}
	if cfg.Healthchecks.URL != "https://hc-ping.com/abc" {
		t.Errorf("Healthchecks.URL = %q, want https://hc-ping.com/abc", cfg.Healthchecks.URL)
	}
}

func TestApplyHealthchecksOffClears(t *testing.T) {
	cfg := &config.Config{}
	cfg.Healthchecks.URL = "https://hc-ping.com/abc"
	if err := applyHealthchecks(cfg, "off"); err != nil {
		t.Fatalf("applyHealthchecks() error = %v, want nil", err)
	}
	if cfg.Healthchecks.URL != "" {
		t.Errorf("Healthchecks.URL = %q, want cleared", cfg.Healthchecks.URL)
	}
}
