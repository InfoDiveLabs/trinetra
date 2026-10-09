//go:build !trinetra_testkeys

package trinetra

import (
	"os"
	"testing"
	"time"
)

// TestE2EHooksIgnoredInReleaseBuild pins that a default (non-testkeys) build
// never honours the update-e2e harness's environment hooks, even when they
// are set: e2eRestartCmd/e2eGuardCmd/e2eGitHubBaseURL are hard-coded no-ops
// in update_e2e_hooks.go, with no env lookup at all, so a real release
// binary cannot be steered by TRINETRA_E2E_RESTART_CMD, TRINETRA_E2E_GUARD_CMD,
// TRINETRA_E2E_GITHUB_BASE_URL, TRINETRA_E2E_UPDATE_LOOP_INTERVAL or
// TRINETRA_E2E_HEALTH_DEADLINE under any circumstances: the self-update
// loop keeps its 5-minute tick and the guard its 90s health window. The
// trinetra_testkeys build (update_e2e_hooks_testkeys.go, exercised by
// test/docker/update) is the only one that reads them.
func TestE2EHooksIgnoredInReleaseBuild(t *testing.T) {
	t.Setenv("TRINETRA_E2E_RESTART_CMD", "pkill -f 'trinetra daemon'")
	t.Setenv("TRINETRA_E2E_GUARD_CMD", "trinetra update guard &")
	t.Setenv("TRINETRA_E2E_GITHUB_BASE_URL", "http://relsrv:8080")
	t.Setenv("TRINETRA_E2E_SWAP_PAUSE_FILE", t.TempDir()+"/paused")
	t.Setenv("TRINETRA_E2E_UPDATE_LOOP_INTERVAL", "1s")
	t.Setenv("TRINETRA_E2E_HEALTH_DEADLINE", "1s")

	if e2eHooksEnabled {
		t.Fatal("e2eHooksEnabled is true in a default (non-testkeys) build")
	}
	if cmd, args, ok := e2eRestartCmd(); ok {
		t.Fatalf("e2eRestartCmd honoured TRINETRA_E2E_RESTART_CMD in a default build: %q %v", cmd, args)
	}
	if cmd, args, ok := e2eGuardCmd(); ok {
		t.Fatalf("e2eGuardCmd honoured TRINETRA_E2E_GUARD_CMD in a default build: %q %v", cmd, args)
	}
	if u := e2eGitHubBaseURL(); u != "" {
		t.Fatalf("e2eGitHubBaseURL honoured TRINETRA_E2E_GITHUB_BASE_URL in a default build: %q", u)
	}
	if d, ok := e2eUpdateLoopInterval(); ok {
		t.Fatalf("e2eUpdateLoopInterval honoured TRINETRA_E2E_UPDATE_LOOP_INTERVAL in a default build: %v", d)
	}
	if d, ok := e2eHealthDeadline(); ok {
		t.Fatalf("e2eHealthDeadline honoured TRINETRA_E2E_HEALTH_DEADLINE in a default build: %v", d)
	}
	if got := updateLoopEvery(); got != 5*time.Minute {
		t.Fatalf("updateLoopEvery() = %v in a default build with the override set, want 5m", got)
	}
	if got := healthDeadline(); got != 90*time.Second {
		t.Fatalf("healthDeadline() = %v in a default build with the override set, want 90s", got)
	}
	start := time.Now()
	e2eAfterFirstRename()
	if _, err := os.Stat(os.Getenv("TRINETRA_E2E_SWAP_PAUSE_FILE")); err == nil || time.Since(start) > time.Second {
		t.Fatal("e2eAfterFirstRename honoured TRINETRA_E2E_SWAP_PAUSE_FILE in a default build")
	}
}

// e2eCrashOnStart must be a compile-time constant in a default build:
// this declaration only compiles if it is one, so no -X stamp can enable the
// crash-on-start hook in a release binary.
const _ = e2eCrashOnStart
