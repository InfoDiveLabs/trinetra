package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyQuietHours applies raw onto cfg's quiet_hours key via the SAME
// validated config.Set setter `serverwatch quiet-hours <HH-HH>|off` uses
// (systemd.go's cmdQuietHours), including that command's literal "off"
// convention for clearing the value: raw == "off" clears quiet_hours,
// anything else is passed straight through to config.Set("quiet_hours",
// raw), which validates the "H-H"/"HH-HH" shape (validateQuietHours,
// internal/config/config.go) and returns its error unapplied.
func applyQuietHours(cfg *config.Config, raw string) error {
	val := raw
	if raw == "off" {
		val = ""
	}
	return cfg.Set("quiet_hours", val)
}
