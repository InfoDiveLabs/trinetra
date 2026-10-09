// Package trinetra: systemd_telegram_test.go covers cmdTelegram set-token's #90 behavior:
// printing the enrollment pin (or a graceful fallback) after saving the token.
package trinetra

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// setTokenTestEnv wires cfgPath to a temp file (seeded with config.Default), captures
// stdout, and restores every overridden package var on cleanup.
func setTokenTestEnv(t *testing.T) *bytes.Buffer {
	t.Helper()
	dir := t.TempDir()
	prevCfgPath := cfgPath
	cfgPath = filepath.Join(dir, "config.json")
	t.Cleanup(func() { cfgPath = prevCfgPath })
	if err := saveCfg(config.Default()); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var out bytes.Buffer
	prevStdout := stdout
	stdout = &out
	t.Cleanup(func() { stdout = prevStdout })

	prevFetch := fetchEnrollmentPINFn
	t.Cleanup(func() { fetchEnrollmentPINFn = prevFetch })

	return &out
}

// TestCmdTelegramSetTokenPrintsPINOnSuccess is the #90 happy path: when
// fetchEnrollmentPINFn yields a pin.
func TestCmdTelegramSetTokenPrintsPINOnSuccess(t *testing.T) {
	out := setTokenTestEnv(t)
	fetchEnrollmentPINFn = func() (string, bool, error) { return "424242", false, nil }

	code := cmdTelegram([]string{"set-token", "abc123"})
	if code != 0 {
		t.Fatalf("cmdTelegram set-token exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "/start 424242") {
		t.Fatalf("stdout = %q, want it to contain the /start <pin> instruction", out.String())
	}

	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.Telegram.Token != "abc123" {
		t.Fatalf("Telegram.Token = %q, want %q (token must still be saved)", got.Telegram.Token, "abc123")
	}
}

// TestCmdTelegramSetTokenFallsBackWhenPINFetchFails is the #90 degraded path: when the pin
// fetch errors (daemon unreachable, e.g. not installed/running).
func TestCmdTelegramSetTokenFallsBackWhenPINFetchFails(t *testing.T) {
	out := setTokenTestEnv(t)
	fetchEnrollmentPINFn = func() (string, bool, error) { return "", false, errors.New("dial unix: no such file or directory") }

	code := cmdTelegram([]string{"set-token", "abc123"})
	if code != 0 {
		t.Fatalf("cmdTelegram set-token exit = %d, want 0 (a pin-fetch failure must never fail set-token)", code)
	}
	if strings.Contains(out.String(), "To finish enrollment") {
		t.Fatalf("stdout = %q, must not print the /start <pin> instruction when the pin fetch failed", out.String())
	}
	if !strings.Contains(out.String(), "journalctl") {
		t.Fatalf("stdout = %q, want the graceful journalctl fallback", out.String())
	}

	got, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.Telegram.Token != "abc123" {
		t.Fatalf("Telegram.Token = %q, want %q (token must still be saved even when the pin fetch fails)", got.Telegram.Token, "abc123")
	}
}

// TestCmdTelegramSetTokenAlreadyEnrolledSkipsPINInstruction: a successful fetch that
// reports enrolled=true (bot already claimed) must not print a /start instruction.
func TestCmdTelegramSetTokenAlreadyEnrolledSkipsPINInstruction(t *testing.T) {
	out := setTokenTestEnv(t)
	fetchEnrollmentPINFn = func() (string, bool, error) { return "", true, nil }

	code := cmdTelegram([]string{"set-token", "abc123"})
	if code != 0 {
		t.Fatalf("cmdTelegram set-token exit = %d, want 0", code)
	}
	if strings.Contains(out.String(), "/start") {
		t.Fatalf("stdout = %q, must not print a /start instruction once already enrolled", out.String())
	}
	if strings.Contains(out.String(), "journalctl") {
		t.Fatalf("stdout = %q, must not print the unreachable-daemon fallback on a successful already-enrolled fetch", out.String())
	}
}
