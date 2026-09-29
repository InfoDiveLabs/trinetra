//go:build trinetra_testkeys

// Package trinetra: update_e2e_hooks_testkeys.go is the trinetra_testkeys
// side of the self-update e2e test hooks (see update_e2e_hooks.go for the
// production no-ops). Only a trinetra_testkeys build -- which already
// trusts update.TestKeySet() instead of the real production release keys
// (internal/update/keys_testkeys.go) -- ever reads these environment
// variables, and only ever on the test/docker/update harness's own
// systemd-less container, which has no real systemd-run/systemctl to drive
// the guard's restart or launch:
//
//   - TRINETRA_E2E_RESTART_CMD, honoured by realGuardDeps().restart
//     (update_guard.go), replaces `systemctl restart trinetra`.
//   - TRINETRA_E2E_GUARD_CMD, honoured by realLaunchGuard (update_cmd.go),
//     replaces `systemd-run ... trinetra update guard`.
//   - TRINETRA_E2E_GITHUB_BASE_URL, honoured by updateSource (update_cmd.go),
//     points GitHubSource at the harness's fake GitHub API (relsrv) instead
//     of the real api.github.com.
package trinetra

import "os"

// e2eHooksEnabled is true only in a trinetra_testkeys build.
const e2eHooksEnabled = true

// e2eRestartCmd reports the shell command TRINETRA_E2E_RESTART_CMD names, if
// set, run via `sh -c`.
func e2eRestartCmd() (cmd string, args []string, ok bool) {
	return shCmd(os.Getenv("TRINETRA_E2E_RESTART_CMD"))
}

// e2eGuardCmd reports the shell command TRINETRA_E2E_GUARD_CMD names, if
// set, run via `sh -c`.
func e2eGuardCmd() (cmd string, args []string, ok bool) {
	return shCmd(os.Getenv("TRINETRA_E2E_GUARD_CMD"))
}

// e2eGitHubBaseURL reports TRINETRA_E2E_GITHUB_BASE_URL, or "" (meaning
// "use the real GitHub API") when it is unset.
func e2eGitHubBaseURL() string { return os.Getenv("TRINETRA_E2E_GITHUB_BASE_URL") }

func shCmd(v string) (cmd string, args []string, ok bool) {
	if v == "" {
		return "", nil, false
	}
	return "sh", []string{"-c", v}, true
}
