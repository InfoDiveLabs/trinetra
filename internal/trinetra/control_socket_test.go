package trinetra

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

// TestServeControlSocketRoundTrip builds a real newInprocAPI over a fake snapshot func, a
// fake getCfg returning config.Default(), a nil store, and a temp stateDir.
func TestServeControlSocketRoundTrip(t *testing.T) {
	// A plain t.TempDir() nests under a per-test-name directory
	// (.../TestServeControlSocketRoundTrip.../001) that, combined with a long $TMPDIR.
	runtimeDir, err := os.MkdirTemp("", "sw-ctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)

	stateDir := t.TempDir()

	fakeSnap := Snapshot{CPU: 42.5, MemPct: 10, TS: 1000}
	getSnap := func() Snapshot { return fakeSnap }
	fakeCfg := config.Default()
	getCfg := func() *config.Config { return fakeCfg }
	reload := func(c *config.Config) error { return nil }

	api := newInprocAPI(getSnap, getCfg, nil, stateDir, reload, nil, &enrollState{})

	stop, _, _, err := serveControlSocket(api)
	if err != nil {
		t.Fatalf("serveControlSocket: %v", err)
	}
	defer stop()

	wantPath := filepath.Join(runtimeDir, "control.sock")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("socket not created at %s: %v", wantPath, err)
	}

	wantTokenPath := filepath.Join(runtimeDir, "token")
	tokenInfo, err := os.Stat(wantTokenPath)
	if err != nil {
		t.Fatalf("token file not created at %s: %v", wantTokenPath, err)
	}
	if mode := tokenInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("token file mode = %o, want 0600", mode)
	}
	tokenBytes, err := os.ReadFile(wantTokenPath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}
	token := string(tokenBytes)
	if token == "" {
		t.Fatalf("token file is empty, want a generated token")
	}

	client, err := control.Dial(wantPath, token)
	if err != nil {
		t.Fatalf("control.Dial: %v", err)
	}
	defer client.Close()

	if _, err := control.Dial(wantPath, "wrong-token"); err == nil {
		t.Errorf("control.Dial with a wrong token succeeded, want an error")
	}

	wantSnap, err := api.Snapshot()
	if err != nil {
		t.Fatalf("api.Snapshot: %v", err)
	}
	gotSnap, err := client.Snapshot()
	if err != nil {
		t.Fatalf("client.Snapshot: %v", err)
	}
	if gotSnap.CPU != wantSnap.CPU || gotSnap.MemPct != wantSnap.MemPct || gotSnap.TS != wantSnap.TS {
		t.Errorf("Snapshot mismatch: got %+v, want %+v", gotSnap, wantSnap)
	}

	gotSeries, err := client.Series("cpu", 0, 100, core.ResAuto)
	if err != nil {
		t.Fatalf("client.Series: %v", err)
	}
	if len(gotSeries) != 0 {
		t.Errorf("Series with nil store = %v, want empty", gotSeries)
	}

	gotCfg, err := client.Config()
	if err != nil {
		t.Fatalf("client.Config: %v", err)
	}
	if gotCfg.FastInterval != fakeCfg.FastInterval {
		t.Errorf("Config mismatch: got FastInterval=%v, want %v", gotCfg.FastInterval, fakeCfg.FastInterval)
	}
}

// newSocketTestAPI builds a minimal newInprocAPI suitable for the
// fail-closed tests below (no snapshot/store behavior is exercised).
func newSocketTestAPI(t *testing.T) core.API {
	t.Helper()
	getSnap := func() Snapshot { return Snapshot{} }
	fakeCfg := config.Default()
	getCfg := func() *config.Config { return fakeCfg }
	reload := func(c *config.Config) error { return nil }
	return newInprocAPI(getSnap, getCfg, nil, t.TempDir(), reload, nil, &enrollState{})
}

// assertSocketFailedClosed asserts serveControlSocket refused to serve: it returned an
// error and a nil stop.
func assertSocketFailedClosed(t *testing.T, runtimeDir string, stop func(), err error) {
	t.Helper()
	if err == nil {
		t.Fatal("serveControlSocket returned nil error, want fail-closed error")
	}
	if stop != nil {
		t.Error("serveControlSocket returned a non-nil stop on failure, want nil")
	}
	socketPath := filepath.Join(runtimeDir, "control.sock")
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Errorf("socket left behind after token failure (stat err = %v); daemon is serving unauthenticated", statErr)
	}
	tokenPath := filepath.Join(runtimeDir, "token")
	if _, statErr := os.Stat(tokenPath); !os.IsNotExist(statErr) {
		t.Errorf("token file left behind after token failure (stat err = %v)", statErr)
	}
}

// TestServeControlSocketFailsClosedOnTokenGenError proves that when the per-launch token
// cannot be GENERATED, the control socket is not served at all.
func TestServeControlSocketFailsClosedOnTokenGenError(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("", "sw-ctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)

	orig := generateTokenFn
	generateTokenFn = func() (string, error) { return "", errors.New("boom: no entropy") }
	t.Cleanup(func() { generateTokenFn = orig })

	stop, _, token, err := serveControlSocket(newSocketTestAPI(t))
	if token != "" {
		t.Errorf("returned token = %q on generation failure, want empty", token)
	}
	assertSocketFailedClosed(t, runtimeDir, stop, err)
}

// TestServeControlSocketFailsClosedOnTokenWriteError proves that when the token WRITE
// fails, the socket is likewise not served unauthenticated (#96).
func TestServeControlSocketFailsClosedOnTokenWriteError(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("", "sw-ctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)

	// Make writeTokenFile fail: os.WriteFile to a path that is a directory errors.
	if err := os.Mkdir(filepath.Join(runtimeDir, "token"), 0o700); err != nil {
		t.Fatalf("pre-creating token dir: %v", err)
	}

	stop, _, token, err := serveControlSocket(newSocketTestAPI(t))
	if token != "" {
		t.Errorf("returned token = %q on write failure, want empty", token)
	}
	if err == nil {
		t.Fatal("serveControlSocket returned nil error on token-write failure, want fail-closed error")
	}
	if stop != nil {
		t.Error("serveControlSocket returned a non-nil stop on write failure, want nil")
	}
	socketPath := filepath.Join(runtimeDir, "control.sock")
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		t.Errorf("socket left behind after token-write failure (stat err = %v); daemon is serving unauthenticated", statErr)
	}
}

// TestServeControlSocketStopRemovesSocketAndTokenFiles proves the stop func
// serveControlSocket returns cleans up both files it created, not just the socket.
func TestServeControlSocketStopRemovesSocketAndTokenFiles(t *testing.T) {
	runtimeDir, err := os.MkdirTemp("", "sw-ctl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)

	stateDir := t.TempDir()
	getSnap := func() Snapshot { return Snapshot{} }
	fakeCfg := config.Default()
	getCfg := func() *config.Config { return fakeCfg }
	reload := func(c *config.Config) error { return nil }
	api := newInprocAPI(getSnap, getCfg, nil, stateDir, reload, nil, &enrollState{})

	stop, _, _, err := serveControlSocket(api)
	if err != nil {
		t.Fatalf("serveControlSocket: %v", err)
	}

	socketPath := filepath.Join(runtimeDir, "control.sock")
	tokenPath := filepath.Join(runtimeDir, "token")
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if _, err := os.Stat(tokenPath); err != nil {
		t.Fatalf("token file not created: %v", err)
	}

	stop()

	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Errorf("socket file still exists after stop(): err = %v", err)
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Errorf("token file still exists after stop(): err = %v", err)
	}
}
