package serverwatch

import (
	"net"
	"os"
)

// sdNotify sends a service-manager notification (sd_notify(3)) over the
// unixgram socket named by $NOTIFY_SOCKET. It is a no-op returning nil when
// NOTIFY_SOCKET is unset -- i.e. not running under systemd, or the unit does
// not enable notifications -- so callers may invoke it unconditionally.
// Stdlib-only; state is a newline-free datagram like "WATCHDOG=1" or "READY=1".
func sdNotify(state string) error {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return nil
	}
	// systemd advertises abstract-namespace sockets with a leading '@', which
	// corresponds to a leading NUL byte in the actual socket address.
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}
