package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyHealthchecks applies raw onto cfg's healthchecks.url key via the SAME config.Set
// setter `trinetra healthchecks set <url>|off` uses (systemd.go's cmdHealthchecks).
func applyHealthchecks(cfg *config.Config, raw string) error {
	val := raw
	if raw == "off" {
		val = ""
	}
	return cfg.Set("healthchecks.url", val)
}
