//go:build trinetra_testkeys

// Package trinetra: update_e2e_hooks_testkeys.go is the trinetra_testkeys side of the
// self-update e2e test hooks (see update_e2e_hooks.go for the production no-ops).
package trinetra

import (
	"os"
	"time"
)

// e2eHooksEnabled is true only in a trinetra_testkeys build.
const e2eHooksEnabled = true

// e2eCrashOnStart is empty unless the e2e harness's "crashes on start" fixture stamps it
// with -ldflags "-X github.com/InfoDiveLabs/trinetra/internal/trinetra.e2eCrashOnStart=1".
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

// e2eUpdateLoopInterval reports TRINETRA_E2E_UPDATE_LOOP_INTERVAL when it
// is a positive duration.
func e2eUpdateLoopInterval() (time.Duration, bool) {
	return envDuration("TRINETRA_E2E_UPDATE_LOOP_INTERVAL")
}

// e2eHealthDeadline reports TRINETRA_E2E_HEALTH_DEADLINE when it is a positive duration.
func e2eHealthDeadline() (time.Duration, bool) {
	return envDuration("TRINETRA_E2E_HEALTH_DEADLINE")
}

func envDuration(name string) (time.Duration, bool) {
	d, err := time.ParseDuration(os.Getenv(name))
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

func shCmd(v string) (cmd string, args []string, ok bool) {
	if v == "" {
		return "", nil, false
	}
	return "sh", []string{"-c", v}, true
}

// e2eAfterFirstRename, when TRINETRA_E2E_SWAP_PAUSE_FILE names a path, creates that file
// right after swapIn's first binary rename and then waits.
func e2eAfterFirstRename() {
	path := os.Getenv("TRINETRA_E2E_SWAP_PAUSE_FILE")
	if path == "" {
		return
	}
	_ = os.WriteFile(path, []byte("paused\n"), 0o644)
	time.Sleep(2 * time.Minute)
}
