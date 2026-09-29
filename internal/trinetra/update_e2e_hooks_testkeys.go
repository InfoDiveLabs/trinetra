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
//   - TRINETRA_E2E_GUARD_CMD, honoured by launchGuardUnit (update_cmd.go),
//     replaces `systemd-run --unit trinetra-update-guard ... <pinned guard>
//     update guard`; the command gets the unit name and the pinned guard
//     binary's path as $1 and $2.
//   - TRINETRA_E2E_SWAP_PAUSE_FILE, honoured by e2eAfterFirstRename
//     (swapIn), pauses an apply right after its first rename.
//   - TRINETRA_E2E_GITHUB_BASE_URL, honoured by updateSource (update_cmd.go),
//     points GitHubSource at the harness's fake GitHub API (relsrv) instead
//     of the real api.github.com.
package trinetra

import (
	"os"
	"time"
)

// e2eHooksEnabled is true only in a trinetra_testkeys build.
const e2eHooksEnabled = true

// e2eCrashOnStart is empty unless the e2e harness's "crashes on start"
// fixture stamps it with
// -ldflags "-X github.com/InfoDiveLabs/trinetra/internal/trinetra.e2eCrashOnStart=1",
// in which case `trinetra daemon` exits immediately (a release that must
// fail its self-update health check). Default builds make it a constant ""
// (update_e2e_hooks.go), so this exists only in trinetra_testkeys builds.
var e2eCrashOnStart string

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

// e2eAfterFirstRename, when TRINETRA_E2E_SWAP_PAUSE_FILE names a path,
// creates that file right after swapIn's first binary rename and then waits
// (up to two minutes) so the harness can SIGKILL the apply mid-swap -- the
// "kill mid-swap, then resume" scenario.
func e2eAfterFirstRename() {
	path := os.Getenv("TRINETRA_E2E_SWAP_PAUSE_FILE")
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte("paused\n"), 0o644)
	time.Sleep(2 * time.Minute)
}
