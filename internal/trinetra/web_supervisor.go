package trinetra

import (
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// This file implements the web supervisor: the goroutine IN the core trinetra daemon that
// keeps the companion trinetra-web binary running as a verified child process.

// supervisedProc is the minimal child-process interface the supervisor needs: wait for it
// to exit, or kill it.
type supervisedProc interface {
	Wait() error
	Kill() error
}

// execProc adapts *exec.Cmd to supervisedProc.
type execProc struct {
	cmd *exec.Cmd
}

func (p *execProc) Wait() error { return p.cmd.Wait() }

// Kill sends an immediate kill (SIGKILL via os.Process.Kill) rather than a graceful
// SIGTERM-then-wait. trinetra-web holds no state of its own.
func (p *execProc) Kill() error { return p.cmd.Process.Kill() }

// startWebProc is the seam over process creation: the default spawns path as a child with
// env as its full environment.
var startWebProc = func(path string, env []string) (supervisedProc, error) {
	cmd := exec.Command(path)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &execProc{cmd: cmd}, nil
}

// resolveWebPlugin is the seam over the front-door trust check, re-run before EVERY
// (re)spawn so a binary swapped between restarts is caught.
var resolveWebPlugin = func() (string, error) {
	return resolveAndVerifyPlugin("web")
}

// supervisorSleep is the seam over the backoff wait.
var supervisorSleep = func(d time.Duration) { time.Sleep(d) }

// supervisorLog is the seam over logging.
var supervisorLog = log.Printf

// timeNow is the seam over the clock, so tests can freeze time for the
// "healthy child resets backoff" logic.
var timeNow = time.Now

const (
	webBackoffMin   = 1 * time.Second
	webBackoffMax   = 30 * time.Second
	webBackoffReset = 60 * time.Second // a child up longer than this is "healthy": reset backoff to min
)

// shouldStartWeb reports whether cmdDaemon should launch the web supervisor:
// only when the control socket is up (the child dials it) and web.enabled.
func shouldStartWeb(cfg *config.Config, socketUp bool) bool {
	return socketUp && cfg.Web.Enabled
}

// startWeb launches the web supervisor goroutine and returns a stop func that signals the
// loop to exit, kills any running child, and blocks until the loop has actually exited.
func startWeb(socketPath, token string) (stop func()) {
	return startCompanion(companionSpec{
		Name:    "web",
		Resolve: func() (string, error) { return resolveWebPlugin() },
		Env: append(os.Environ(),
			"TRINETRA_CONTROL_SOCKET="+socketPath,
			"TRINETRA_CONTROL_TOKEN="+token,
			// Compat: kept for one release so a pre-rename serverwatch-web binary (which only reads
			// the old names) still works when spawned by a new core.
			"SERVERWATCH_CONTROL_SOCKET="+socketPath,
			"SERVERWATCH_CONTROL_TOKEN="+token,
		),
		Start: func(path string, env []string) (supervisedProc, error) { return startWebProc(path, env) },
	})
}

// nextBackoff doubles d, capped at webBackoffMax.
func nextBackoff(d time.Duration) time.Duration {
	return min(d*2, webBackoffMax)
}

// sleepOrStop sleeps d via supervisorSleep, but returns false immediately (without waiting
// for the sleep to finish) if stopCh closes first.
func sleepOrStop(d time.Duration, stopCh <-chan struct{}) bool {
	slept := make(chan struct{})
	sleep := supervisorSleep
	go func() {
		sleep(d)
		close(slept)
	}()
	select {
	case <-slept:
		return true
	case <-stopCh:
		return false
	}
}
