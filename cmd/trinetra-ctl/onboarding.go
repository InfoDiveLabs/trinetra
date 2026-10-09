// onboarding.go holds the first-run onboarding flow's PURE logic: whether it's needed at
// all, and the config mutation that saves the captured bot token, both unit-tested.
package main

import "github.com/InfoDiveLabs/trinetra/internal/config"

// applyOnboardToken sets telegram.token through the same validated setter
// `trinetra telegram set-token` uses.
func applyOnboardToken(cfg *config.Config, token string) error {
	return cfg.Set("telegram.token", token)
}
