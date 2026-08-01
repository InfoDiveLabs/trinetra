// Package serverwatch: control_socket.go starts the control-socket server
// (internal/control) over the daemon's own core.API (newInprocAPI,
// coreapi_inproc.go), the transport that lets a separate-process consumer
// (S3's serverwatch-ctl, S4's serverwatch-web) talk to the running daemon
// without going through the CLI's file-backed core.API.
//
// This file is deliberately UNTAGGED, same reasoning as coreapi_inproc.go:
// internal/control imports only stdlib + internal/core + internal/config
// (see internal/control/doc.go), so serving it never pulls internal/web's
// third-party dependencies into the default build. cmdDaemon calls this in
// both build variants.
package serverwatch

import (
	"net"
	"os"
	"path/filepath"

	"serverwatch/internal/control"
	"serverwatch/internal/core"
)

// defaultRuntimeDir is where the control socket lives when systemd hasn't
// set RUNTIME_DIRECTORY (e.g. running the daemon by hand outside the unit).
// The systemd unit's RuntimeDirectory=serverwatch (systemd.go's renderUnit)
// makes systemd create /run/serverwatch itself (tmpfs, 0755, root, and
// auto-removed on stop) and export RUNTIME_DIRECTORY=/run/serverwatch to the
// service -- controlSocketPath below prefers that env var whenever it is
// set, so under systemd this constant is never actually used; the
// MkdirAll fallback in serveControlSocket exists for the by-hand case.
const defaultRuntimeDir = "/run/serverwatch"

// controlSocketPath resolves where the control socket should be created:
// $RUNTIME_DIRECTORY/control.sock when systemd (or a test) has set that
// env var, else defaultRuntimeDir/control.sock. Tests set RUNTIME_DIRECTORY
// to a t.TempDir() so this never needs root or touches the real /run.
func controlSocketPath() string {
	dir := os.Getenv("RUNTIME_DIRECTORY")
	if dir == "" {
		dir = defaultRuntimeDir
	}
	return filepath.Join(dir, "control.sock")
}

// serveControlSocket starts control.Serve against api on a unix socket
// (controlSocketPath) and returns a stop func that shuts it down. Like
// maybeStartWeb, the control socket is an enhancement: cmdDaemon treats any
// error from this as non-fatal (log to stderr, keep running without it)
// rather than a reason to crash-loop the daemon -- callers should follow
// that same pattern rather than propagating a failure upward.
//
// Path setup: os.MkdirAll(0o700) is a best-effort attempt to create the
// runtime directory when it doesn't already exist (the by-hand,
// no-RUNTIME_DIRECTORY case). Under systemd's RuntimeDirectory=serverwatch
// the directory already exists, but systemd creates it 0755 root -- so it
// is explicitly chmod'd to 0700 here too (before the socket is bound),
// closing the window where a non-owner could connect between Listen and
// the socket's own Chmod below. Any stale socket left behind by an
// unclean shutdown is removed before binding -- net.Listen("unix", ...)
// fails with "address already in use" over a leftover socket file
// otherwise. The listener is chmod 0600 after creation (net.Listen honors
// the umask, not an explicit mode) so only the daemon's own user can
// connect; a forthcoming per-launch token (S3) is the actual auth, this is
// defense-in-depth against other local users on multi-user hosts.
func serveControlSocket(api core.API) (func(), error) {
	path := controlSocketPath()
	dir := filepath.Dir(path)

	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700)
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	go control.Serve(api, ln)

	stop := func() {
		ln.Close()
		os.Remove(path)
	}
	return stop, nil
}
