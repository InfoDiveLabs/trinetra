package serverwatch

import (
	"log"
	"os"
	"os/exec"
	"time"

	"serverwatch/internal/config"
)

// This file implements the web supervisor: the goroutine IN the core
// serverwatch daemon that keeps the companion serverwatch-web binary
// running as a verified child process. It reuses the front-door trust
// check, resolveAndVerifyPlugin("web") (plugin_launch.go), before every
// (re)spawn, so a binary swapped in between restarts is caught the same
// way a swap before the first spawn would be.
//
// The child reads its control socket path and auth token from the env vars
// SERVERWATCH_CONTROL_SOCKET and SERVERWATCH_CONTROL_TOKEN and sources ALL
// of its config over that socket, so this supervisor only ever needs to
// set those two env vars; it never passes a listen address or any other
// config.
//
// Stdlib only: this file must not import anything outside the standard
// library. TestDefaultBuildIsStdlibOnly (buildtag_test.go) enforces that
// the default (untagged) build of cmd/serverwatch never pulls in
// third-party packages, and this file is part of that build.

// supervisedProc is the minimal child-process interface the supervisor
// needs: wait for it to exit, or kill it. It exists so tests can swap in a
// fake process instead of a real os/exec.Cmd.
type supervisedProc interface {
	Wait() error
	Kill() error
}

// execProc adapts *exec.Cmd to supervisedProc.
type execProc struct {
	cmd *exec.Cmd
}

func (p *execProc) Wait() error { return p.cmd.Wait() }

// Kill sends an immediate kill (SIGKILL via os.Process.Kill) rather than a
// graceful SIGTERM-then-wait. serverwatch-web holds no state of its own
// (it sources everything from the control socket on every connection), so
// there is nothing for it to flush on shutdown, and a plain Kill keeps this
// seam simple: { Wait; Kill }, nothing more.
func (p *execProc) Kill() error { return p.cmd.Process.Kill() }

// startWebProc is the seam over process creation: the default spawns path
// as a child with env as its full environment (which the caller has
// already built from os.Environ() plus the two SERVERWATCH_CONTROL_* vars),
// wiring the child's stdout/stderr to the daemon's own so its logs show up
// wherever the daemon's do. Overridden in tests to avoid spawning a real
// process.
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

// resolveWebPlugin is the seam over the front-door trust check, re-run
// before EVERY (re)spawn so a binary swapped between restarts is caught.
// Overridden in tests to avoid touching the filesystem.
var resolveWebPlugin = func() (string, error) {
	return resolveAndVerifyPlugin("web")
}

// supervisorSleep is the seam over the backoff wait. Overridden in tests so
// backoff is instant and durations can be recorded instead of actually
// waited out.
var supervisorSleep = func(d time.Duration) { time.Sleep(d) }

// supervisorLog is the seam over logging. Overridden in tests to capture
// messages instead of writing to the real log.
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

// startWeb launches the web supervisor goroutine and returns a stop func
// that signals the loop to exit, kills any running child, and blocks until
// the loop has actually exited. Mirrors the daemon's existing
// serveControlSocket pattern (control_socket.go): the caller defers the
// returned stop.
func startWeb(socketPath, token string) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go superviseWeb(socketPath, token, stopCh, done)
	return func() {
		close(stopCh)
		<-done
	}
}

// superviseWeb resolves and spawns serverwatch-web, then restarts it with
// capped backoff whenever it exits on its own, until stopCh closes. Every
// (re)spawn re-runs resolveWebPlugin so a binary swapped in between
// restarts is caught the same way a swap before the first spawn would be.
func superviseWeb(socketPath, token string, stopCh <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	env := append(os.Environ(),
		"SERVERWATCH_CONTROL_SOCKET="+socketPath,
		"SERVERWATCH_CONTROL_TOKEN="+token,
	)

	backoff := webBackoffMin
	for {
		select {
		case <-stopCh:
			return
		default:
		}

		path, err := resolveWebPlugin()
		if err != nil {
			supervisorLog("web: refusing to start serverwatch-web: %v (run `serverwatch install` after a rebuild)", err)
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		proc, err := startWebProc(path, env)
		if err != nil {
			supervisorLog("web: failed to spawn %s: %v", path, err)
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		start := timeNow()
		waitCh := make(chan error, 1)
		go func() { waitCh <- proc.Wait() }()
		select {
		case <-stopCh:
			_ = proc.Kill()
			<-waitCh
			return
		case werr := <-waitCh:
			supervisorLog("web: serverwatch-web exited: %v", werr)
			if timeNow().Sub(start) >= webBackoffReset {
				backoff = webBackoffMin
			}
			if !sleepOrStop(backoff, stopCh) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// nextBackoff doubles d, capped at webBackoffMax.
func nextBackoff(d time.Duration) time.Duration {
	return min(d*2, webBackoffMax)
}

// sleepOrStop sleeps d via supervisorSleep, but returns false immediately
// (without waiting for the sleep to finish) if stopCh closes first. It
// returns true if the sleep ran to completion. Running supervisorSleep in
// its own goroutine and selecting against stopCh is what makes stop()
// return promptly even in the middle of a backoff wait.
func sleepOrStop(d time.Duration, stopCh <-chan struct{}) bool {
	slept := make(chan struct{})
	go func() {
		supervisorSleep(d)
		close(slept)
	}()
	select {
	case <-slept:
		return true
	case <-stopCh:
		return false
	}
}
