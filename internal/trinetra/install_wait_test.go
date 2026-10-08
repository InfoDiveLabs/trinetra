package trinetra

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installWaitSock returns a short unix socket path (sun_path is capped near
// 104 bytes on darwin, which a nested t.TempDir() can exceed).
func installWaitSock(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "c.sock")
}

// install must not report "started" before the daemon can be reached
// (#162): waitForDaemon returns once the control socket accepts.
func TestWaitForDaemonReturnsWhenSocketAccepts(t *testing.T) {
	sock := installWaitSock(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		t.Cleanup(func() { ln.Close() })
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := waitForDaemon(sock, func() serviceState { return serviceStarting }, 5*time.Second); err != nil {
		t.Fatalf("waitForDaemon = %v, want nil", err)
	}
}

// A service that crashed (systemd is auto-restarting it, or gave up) is
// reported at once instead of waiting out the whole timeout.
func TestWaitForDaemonReportsCrashedService(t *testing.T) {
	sock := installWaitSock(t)
	start := time.Now()
	err := waitForDaemon(sock, func() serviceState { return serviceDown }, 10*time.Second)
	if !errors.Is(err, errServiceNotUp) {
		t.Fatalf("waitForDaemon = %v, want errServiceNotUp", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("crash reported after %v, want promptly", el)
	}
}

// A daemon that is alive but slow to open its socket ends in
// errServiceSlow after the timeout, not a failure.
func TestWaitForDaemonSlowStart(t *testing.T) {
	sock := installWaitSock(t)
	err := waitForDaemon(sock, func() serviceState { return serviceStarting }, 300*time.Millisecond)
	if !errors.Is(err, errServiceSlow) {
		t.Fatalf("waitForDaemon = %v, want errServiceSlow", err)
	}
}

func TestParseServiceState(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want serviceState
	}{
		{"ActiveState=active\nSubState=running\n", serviceStarting},
		{"ActiveState=activating\nSubState=auto-restart\n", serviceDown},
		{"ActiveState=failed\nSubState=failed\n", serviceDown},
		{"ActiveState=inactive\nSubState=dead\n", serviceDown},
		{"ActiveState=activating\nSubState=start\n", serviceStarting},
		{"", serviceStarting},
	} {
		if got := parseServiceState(tc.out); got != tc.want {
			t.Errorf("parseServiceState(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}
