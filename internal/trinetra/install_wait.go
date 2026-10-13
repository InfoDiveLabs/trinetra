package trinetra

import (
	"errors"
	"net"
	"strings"
	"time"
)

// systemctl restart of the Type=simple unit returns before the daemon has
// opened its control socket, so install waits for it (#162).
const daemonReadyWait = 20 * time.Second

var (
	errServiceNotUp = errors.New("the trinetra service is not staying up")
	errServiceSlow  = errors.New("the trinetra daemon is still starting")
)

type serviceState int

const (
	serviceStarting serviceState = iota // active or activating: keep waiting
	serviceDown                         // failed, stopped or auto-restarting
)

var waitForDaemonFn = func() error {
	return waitForDaemon(controlSocketPath(), systemdServiceState, daemonReadyWait)
}

// waitForDaemon polls until sock accepts, the service is down
// (errServiceNotUp), or wait elapses (errServiceSlow).
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

// systemdServiceState reports starting if systemctl itself fails, so the
// wait degrades to a timeout rather than a false "crashed".
func systemdServiceState() serviceState {
	out, err := osExec{}.Run("systemctl", "show", "trinetra", "-p", "ActiveState", "-p", "SubState")
	if err != nil {
		return serviceStarting
	}
	return parseServiceState(string(out))
}

// parseServiceState: with Restart=always a crashing daemon sits in
// auto-restart rather than failed, so that counts as down too.
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
