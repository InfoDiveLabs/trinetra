package main

import (
	"context"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

func TestNeedsOnboardingNilConfig(t *testing.T) {
	if !needsOnboarding(nil) {
		t.Error("needsOnboarding(nil) = false, want true")
	}
}

func TestNeedsOnboardingNoToken(t *testing.T) {
	if !needsOnboarding(&config.Config{}) {
		t.Error("needsOnboarding(no token) = false, want true")
	}
}

func TestNeedsOnboardingTokenNotEnrolled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.Token = "abc"
	if !needsOnboarding(cfg) {
		t.Error("needsOnboarding(token set, no chat id) = false, want true")
	}
}

func TestNeedsOnboardingEnrolled(t *testing.T) {
	cfg := &config.Config{}
	cfg.Telegram.Token = "abc"
	cfg.Telegram.ChatID = "123"
	if needsOnboarding(cfg) {
		t.Error("needsOnboarding(token+chat id set) = true, want false")
	}
}

func TestApplyOnboardTokenSetsToken(t *testing.T) {
	cfg := &config.Config{}
	if err := applyOnboardToken(cfg, "mytoken"); err != nil {
		t.Fatalf("applyOnboardToken() error = %v, want nil", err)
	}
	if cfg.Telegram.Token != "mytoken" {
		t.Errorf("Telegram.Token = %q, want mytoken", cfg.Telegram.Token)
	}
}

// TestOnboardFlowAgainstFakeAPI drives the whole pure-ish flow end to end
// against a fakeAPI, the shape the task spec asks for: capture a token,
// apply it, fetch the pin (not yet enrolled), then simulate the daemon
// completing enrollment (fakeAPI.enrollEnrolled flips) and fetch again.
// This exercises exactly the sequence onboard_ui.go's Bubble Tea glue
// drives via applyOnboardTokenCmd/fetchOnboardPINCmd, without any terminal
// or tea.Program involved.
func TestOnboardFlowAgainstFakeAPI(t *testing.T) {
	api := &fakeAPI{cfg: &config.Config{}, enrollPIN: "4821", enrollEnrolled: false}

	// 1. capture + apply the token.
	cfg, err := api.Config()
	if err != nil {
		t.Fatalf("Config() error = %v", err)
	}
	if err := applyOnboardToken(cfg, "mytoken"); err != nil {
		t.Fatalf("applyOnboardToken() error = %v", err)
	}
	if err := api.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	if api.applied.Telegram.Token != "mytoken" {
		t.Fatalf("applied token = %q, want mytoken", api.applied.Telegram.Token)
	}

	// 2. pin shown, not yet enrolled.
	pin, enrolled, err := api.EnrollmentPIN(context.Background())
	if err != nil {
		t.Fatalf("EnrollmentPIN() error = %v", err)
	}
	if pin != "4821" || enrolled {
		t.Fatalf("EnrollmentPIN() = (%q, %v), want (4821, false)", pin, enrolled)
	}

	// 3. daemon completes enrollment; polling again detects it.
	api.enrollEnrolled = true
	api.enrollPIN = ""
	pin, enrolled, err = api.EnrollmentPIN(context.Background())
	if err != nil {
		t.Fatalf("EnrollmentPIN() (poll) error = %v", err)
	}
	if pin != "" || !enrolled {
		t.Fatalf("EnrollmentPIN() (poll) = (%q, %v), want (\"\", true)", pin, enrolled)
	}
	if api.enrollCalls != 2 {
		t.Errorf("enrollCalls = %d, want 2 (initial + poll)", api.enrollCalls)
	}
}
