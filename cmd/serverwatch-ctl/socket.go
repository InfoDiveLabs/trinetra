package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultRuntimeDir mirrors internal/serverwatch's constant of the same name:
// where the control socket and its sibling token file live when systemd has
// not exported RUNTIME_DIRECTORY (the by-hand case). Duplicated here rather
// than imported because internal/serverwatch is the daemon's own package
// (untagged, but it pulls in the whole daemon) and this client only needs the
// two path strings.
const defaultRuntimeDir = "/run/serverwatch"

// resolveSocketPath decides which control socket to dial, highest priority
// first: the --socket flag value (flagVal), then $SERVERWATCH_CONTROL_SOCKET,
// then $RUNTIME_DIRECTORY/control.sock, then defaultRuntimeDir/control.sock.
// This mirrors internal/serverwatch/control_socket.go's controlSocketPath so
// the client dials exactly where the daemon serves, with flag/env overrides
// on top for testing and non-systemd layouts.
func resolveSocketPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("SERVERWATCH_CONTROL_SOCKET"); env != "" {
		return env
	}
	dir := os.Getenv("RUNTIME_DIRECTORY")
	if dir == "" {
		dir = defaultRuntimeDir
	}
	return filepath.Join(dir, "control.sock")
}

// resolveTokenPath decides which token file to read, highest priority first:
// the --token flag value (flagVal), then $SERVERWATCH_CONTROL_TOKEN, then the
// sibling "token" file next to the resolved socket. Keying the default off the
// socket's own directory keeps the pair consistent when --socket points
// somewhere non-default (mirroring the daemon, which writes both into the same
// runtime directory).
func resolveTokenPath(flagVal, socketPath string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("SERVERWATCH_CONTROL_TOKEN"); env != "" {
		return env
	}
	return filepath.Join(filepath.Dir(socketPath), "token")
}

// resolveToken reads the per-launch control token from the resolved token
// file. A missing token file is treated as "no token" (empty string, no
// error) rather than fatal: the daemon serves with no auth when it could not
// generate or persist a token (see serveControlSocket's tolerant handling),
// and control.Dial presents "" in that case. Any other read error (e.g. a
// present-but-unreadable file, the usual root-owned-0600 permission case) is
// surfaced so the operator learns they cannot authenticate rather than seeing
// a bare connection-refused-style failure.
func resolveToken(flagVal, socketPath string) (string, error) {
	path := resolveTokenPath(flagVal, socketPath)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
