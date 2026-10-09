// onboarding.go holds the first-run onboarding flow's PURE logic: whether it's needed at
// all, and the config mutation that saves the captured bot token, both unit-tested.
package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// needsOnboarding reports whether ctl should offer the guided first-run flow: telegram
// isn't configured at all (no token) or is configured but not yet enrolled (no chat id).
func needsOnboarding(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	return cfg.Telegram.Token == "" || cfg.Telegram.ChatID == ""
}

// applyOnboardToken sets telegram.token on cfg via the SAME validated config.Set setter
// `trinetra telegram set-token` uses (systemd.go's cmdTelegram).
func applyOnboardToken(cfg *config.Config, token string) error {
	return cfg.Set("telegram.token", token)
}
