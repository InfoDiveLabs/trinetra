package trinetra

import (
	"errors"
	"net"
	"strings"
	"time"
)

// install's `systemctl restart` of the Type=simple unit returns as soon as
// the process is forked, while the daemon only creates its control socket
// late in startup. Without a wait, install reports "started" and the
// `trinetra cli` an operator runs next finds no socket (#162).

// daemonReadyWait bounds how long install waits for the daemon.
const daemonReadyWait = 20 * time.Second

var (
	// errServiceNotUp: the service stopped or systemd is restarting it.
	errServiceNotUp = errors.New("the trinetra service is not staying up")
	// errServiceSlow: the service is running but has not opened its control
	// socket within the wait.
	errServiceSlow = errors.New("the trinetra daemon is still starting")
)

type serviceState int

const (
	serviceStarting serviceState = iota // active or activating: keep waiting
	serviceDown                         // failed, stopped or auto-restarting
)

// waitForDaemonFn is the seam cmdInstall calls; tests replace it.
var waitForDaemonFn = func() error {
	return waitForDaemon(controlSocketPath(), systemdServiceState, daemonReadyWait)
}

// waitForDaemon polls until the control socket at sock accepts a
// connection (nil), state reports the service down (errServiceNotUp), or wait
// elapses (errServiceSlow).
func waitForDaemon(sock string, state func() serviceState, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		if c, err := net.DialTimeout("unix", sock, time.Second); err == nil {
			c.Close()
			return nil
		}
		if state() == serviceDown {
			return errServiceNotUp
		}
		left := time.Until(deadline)
		if left <= 0 {
			return errServiceSlow
		}
		time.Sleep(min(250*time.Millisecond, left))
	}
}

// systemdServiceState asks systemd how trinetra.service is doing. If
// systemctl itself fails, it reports starting, so the wait degrades to a
// plain timeout rather than a false "crashed".
func systemdServiceState() serviceState {
	out, err := osExec{}.Run("systemctl", "show", "trinetra", "-p", "ActiveState", "-p", "SubState")
	if err != nil {
		return serviceStarting
	}
	return parseServiceState(string(out))
}

// parseServiceState reads `systemctl show -p ActiveState -p SubState`.
// Restart=always means a crashing daemon sits in activating/auto-restart
// rather than failed, so that counts as down too.
func parseServiceState(out string) serviceState {
	var active, sub string
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			active = v
		case "SubState":
			sub = v
		}
	}
	if active == "failed" || active == "inactive" || active == "deactivating" || sub == "auto-restart" {
		return serviceDown
	}
	return serviceStarting
}
