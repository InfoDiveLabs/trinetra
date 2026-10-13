package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyQuietHours applies raw onto cfg's quiet_hours key via the SAME validated config.Set
// setter `trinetra quiet-hours <HH-HH>|off` uses (systemd.go's cmdQuietHours).
func applyQuietHours(cfg *config.Config, raw string) error {
	val := raw
	if raw == "off" {
		val = ""
	}
	return cfg.Set("quiet_hours", val)
}
