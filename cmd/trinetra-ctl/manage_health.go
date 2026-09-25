package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyHealthchecks applies raw onto cfg's healthchecks.url key via the
// SAME config.Set setter `serverwatch healthchecks set <url>|off` uses
// (systemd.go's cmdHealthchecks), including that command's literal "off"
// convention for clearing the value: raw == "off" clears healthchecks.url,
// anything else is stored as the URL verbatim (config.Set("healthchecks.url",
// ...) does no URL-shape validation today, matching cmdHealthchecks).
func applyHealthchecks(cfg *config.Config, raw string) error {
	val := raw
	if raw == "off" {
		val = ""
	}
	return cfg.Set("healthchecks.url", val)
}
