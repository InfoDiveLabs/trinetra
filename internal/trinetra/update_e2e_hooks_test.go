//go:build !trinetra_testkeys

package trinetra

import "testing"

// TestE2EHooksIgnoredInReleaseBuild pins that a default (non-testkeys) build
// never honours the update-e2e harness's environment hooks, even when they
// are set: e2eRestartCmd/e2eGuardCmd/e2eGitHubBaseURL are hard-coded no-ops
// in update_e2e_hooks.go, with no env lookup at all, so a real release
// binary cannot be steered by TRINETRA_E2E_RESTART_CMD, TRINETRA_E2E_GUARD_CMD
// or TRINETRA_E2E_GITHUB_BASE_URL under any circumstances. The
// trinetra_testkeys build (update_e2e_hooks_testkeys.go, exercised by
// test/docker/update) is the only one that reads them.
func TestE2EHooksIgnoredInReleaseBuild(t *testing.T) {
	t.Setenv("TRINETRA_E2E_RESTART_CMD", "pkill -f 'trinetra daemon'")
	t.Setenv("TRINETRA_E2E_GUARD_CMD", "trinetra update guard &")
	t.Setenv("TRINETRA_E2E_GITHUB_BASE_URL", "http://relsrv:8080")

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
}
