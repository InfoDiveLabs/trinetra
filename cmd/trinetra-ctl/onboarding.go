// onboarding.go saves the bot token captured by the Telegram setup screens
// (onboard_ui.go). Telegram is optional: the guided first run lives in
// firstrun.go.
package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyOnboardToken sets telegram.token through the same validated setter
// `trinetra telegram set-token` uses.
func applyOnboardToken(cfg *config.Config, token string) error {
	return cfg.Set("telegram.token", token)
}
