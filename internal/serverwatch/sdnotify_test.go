package serverwatch

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSdNotifyUnsetIsNoop asserts sdNotify is a silent no-op when
// $NOTIFY_SOCKET is unset (i.e. not running under systemd), so callers may
// invoke it unconditionally.
func TestSdNotifyUnsetIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := sdNotify("WATCHDOG=1"); err != nil {
		t.Fatalf("sdNotify with unset NOTIFY_SOCKET: got %v, want nil", err)
	}
}

// TestSdNotifyHappyPath asserts sdNotify writes the given state string as a
// single datagram to the unixgram socket named by $NOTIFY_SOCKET.
func TestSdNotifyHappyPath(t *testing.T) {
	// Keep the socket path short: temp dirs from t.TempDir() can exceed the
	// ~108-char sun_path limit on some platforms/CI layouts.
	sockPath := filepath.Join(os.TempDir(), "sw-notify-test.sock")
	_ = os.Remove(sockPath)
	if len(sockPath) > 100 {
		t.Skipf("socket path too long for sun_path: %q", sockPath)
	}

	l, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatalf("ListenUnixgram: %v", err)
	}
	t.Cleanup(func() {
		l.Close()
		os.Remove(sockPath)
	})

	t.Setenv("NOTIFY_SOCKET", sockPath)

	if err := sdNotify("WATCHDOG=1"); err != nil {
		t.Fatalf("sdNotify: %v", err)
	}

	_ = l.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := l.Read(buf)
	if err != nil {
		t.Fatalf("reading datagram: %v", err)
	}
	if got := string(buf[:n]); got != "WATCHDOG=1" {
		t.Fatalf("datagram = %q, want %q", got, "WATCHDOG=1")
	}
}
