// Package trinetra: control_socket.go starts the control-socket server (internal/control)
// over the daemon's own core.API (newInprocAPI, coreapi_inproc.go).
package trinetra

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// defaultRuntimeDir is where the control socket lives when systemd hasn't set
// RUNTIME_DIRECTORY (e.g. running the daemon by hand outside the unit).
const defaultRuntimeDir = "/run/trinetra"

// controlSocketPath resolves where the control socket should be created:
// $RUNTIME_DIRECTORY/control.sock when systemd (or a test) has set that env var.
func controlSocketPath() string {
	dir := os.Getenv("RUNTIME_DIRECTORY")
	if dir == "" {
		dir = defaultRuntimeDir
	}
	return filepath.Join(dir, "control.sock")
}

// controlTokenPath resolves where the per-launch control-socket token is written, mirroring
// controlSocketPath: same directory, named "token" instead of "control.sock".
func controlTokenPath() string {
	dir := os.Getenv("RUNTIME_DIRECTORY")
	if dir == "" {
		dir = defaultRuntimeDir
	}
	return filepath.Join(dir, "token")
}

// generateToken returns a fresh 32-hex-character (16 random byte) token for
// authenticating clients against this daemon launch's control socket.
func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// generateTokenFn is the seam serveControlSocket calls to mint the per-launch token; it is
// a package var.
var generateTokenFn = generateToken

// writeTokenFile writes token to path with mode 0600, regardless of the process umask.
func writeTokenFile(path, token string) error {
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// serveControlSocket starts control.Serve against api on a unix socket (controlSocketPath)
// and returns a stop func that shuts it down.
func serveControlSocket(api core.API) (stop func(), socketPath, token string, err error) {
	path := controlSocketPath()
	dir := filepath.Dir(path)

	_ = os.MkdirAll(dir, 0o700)
	_ = os.Chmod(dir, 0o700)
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, "", "", err
	}

	// The token is the actual auth for the socket (the 0600 mode above is defense-in-depth
	// against other local users).
	tokenPath := controlTokenPath()
	token, err = generateTokenFn()
	if err != nil {
		ln.Close()
		os.Remove(path)
		return nil, "", "", fmt.Errorf("control: generating token (not serving socket): %w", err)
	}
	if err := writeTokenFile(tokenPath, token); err != nil {
		ln.Close()
		os.Remove(path)
		return nil, "", "", fmt.Errorf("control: writing token file %s (not serving socket): %w", tokenPath, err)
	}

	go control.Serve(api, ln, token)

	stop = func() {
		ln.Close()
		os.Remove(path)
		os.Remove(tokenPath)
	}
	return stop, path, token, nil
}
