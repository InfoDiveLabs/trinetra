package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultRuntimeDir mirrors internal/trinetra's constant of the same name.
const defaultRuntimeDir = "/run/trinetra"

// resolveSocketPath decides which control socket to dial, highest priority
// first: the --socket flag value (flagVal), then $TRINETRA_CONTROL_SOCKET,
// then (compat, for one release) $SERVERWATCH_CONTROL_SOCKET, then
// $RUNTIME_DIRECTORY/control.sock, then defaultRuntimeDir/control.sock.
// This mirrors internal/trinetra/control_socket.go's controlSocketPath so
// the client dials exactly where the daemon serves, with flag/env overrides
// on top for testing and non-systemd layouts.
func resolveSocketPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if env := os.Getenv("TRINETRA_CONTROL_SOCKET"); env != "" {
		return env
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

// resolveTokenFile decides which token FILE to read, highest priority first: the --token
// flag value (a file path), then the sibling "token" file next to the resolved socket.
func resolveTokenFile(flagVal, socketPath string) string {
	if flagVal != "" {
		return flagVal
	}
	return filepath.Join(filepath.Dir(socketPath), "token")
}

// resolveToken resolves the per-launch control token, highest priority first:
// a --token flag naming a token FILE; then $TRINETRA_CONTROL_TOKEN, then
// (compat, for one release) $SERVERWATCH_CONTROL_TOKEN, either of which
// carries the token VALUE directly (this is how the daemon's web supervisor
// and the `trinetra cli`/`web` front-doors hand the per-launch token to a
// spawned plugin, and how trinetra-web reads it too) -- it is used verbatim,
// never as a file path; then the sibling "token" file next to the resolved
// socket. A missing token file is treated as "no token" (empty string, no
// error) rather than fatal: the daemon serves with no auth when it could not
// generate or persist a token (see serveControlSocket's tolerant handling),
// and control.Dial presents "" in that case. Any other read error (e.g. a
// present-but-unreadable file, the usual root-owned-0600 permission case) is
// surfaced so the operator learns they cannot authenticate rather than seeing
// a bare connection-refused-style failure.
func resolveToken(flagVal, socketPath string) (string, error) {
	if flagVal == "" {
		if env := os.Getenv("TRINETRA_CONTROL_TOKEN"); env != "" {
			return env, nil
		}
		if env := os.Getenv("SERVERWATCH_CONTROL_TOKEN"); env != "" {
			return env, nil
		}
	}
	b, err := os.ReadFile(resolveTokenFile(flagVal, socketPath))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
