package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultRuntimeDir mirrors internal/trinetra's constant of the same name.
const defaultRuntimeDir = "/run/trinetra"

// resolveSocketPath decides which control socket to dial, highest priority first: the
// --socket flag value (flagVal), then $TRINETRA_CONTROL_SOCKET.
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

// resolveToken resolves the per-launch control token, highest priority first: a --token
// flag naming a token FILE; then $TRINETRA_CONTROL_TOKEN.
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
