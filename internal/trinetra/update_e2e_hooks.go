//go:build !trinetra_testkeys

// Package trinetra: update_e2e_hooks.go is the default (production) side of the self-update
// e2e test hooks.
package trinetra

import "time"

// e2eHooksEnabled is false in every production build.
const e2eHooksEnabled = false

// e2eCrashOnStart is a constant "" in a default build, so cmdDaemon's crash-on-start branch
// is compiled out and no -ldflags -X stamp can turn it on in a release binary.
const e2eCrashOnStart = ""

// e2eRestartCmd always reports "not set" in a default build: the guard's restart
// (update_guard.go's realGuardDeps) always shells out to `systemctl restart trinetra`.
func e2eRestartCmd() (cmd string, args []string, ok bool) { return "", nil, false }

// e2eGuardCmd always reports "not set" in a default build: launchGuard
// (update_cmd.go's launchGuardUnit) always shells out to `systemd-run`.
func e2eGuardCmd() (cmd string, args []string, ok bool) { return "", nil, false }

// e2eGitHubBaseURL always returns "" in a default build: updateSource
// (update_cmd.go) always talks to the real GitHub API.
func e2eGitHubBaseURL() string { return "" }

// e2eUpdateLoopInterval always reports "not set" in a default build: the self-update loop
// (update_daemon.go's updateLoopEvery) always ticks every updateLoopInterval (5 minutes).
func e2eUpdateLoopInterval() (time.Duration, bool) { return 0, false }

// e2eHealthDeadline always reports "not set" in a default build: a pending update always
// gets updateHealthDeadline (90s) to prove itself (update_apply.go's healthDeadline).
func e2eHealthDeadline() (time.Duration, bool) { return 0, false }

// e2eAfterFirstRename is a no-op in a default build.
func e2eAfterFirstRename() {}
