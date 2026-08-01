package serverwatch

import (
	"os"
	"path/filepath"
	"testing"

	"serverwatch/internal/config"
	"serverwatch/internal/control"
	"serverwatch/internal/core"
)

// TestServeControlSocketRoundTrip builds a real newInprocAPI over a fake
// snapshot func, a fake getCfg returning config.Default(), a nil store, and
// a temp stateDir, serves it on a control socket resolved under
// RUNTIME_DIRECTORY (so the test needs no root), dials it, and asserts
// Snapshot/Series/Config round-trip through the socket exactly as calling
// the in-process api directly would -- proving the transport adds no
// behavior change of its own.
func TestServeControlSocketRoundTrip(t *testing.T) {
	// A plain t.TempDir() nests under a per-test-name directory
	// (.../TestServeControlSocketRoundTrip.../001) that, combined with a long
	// $TMPDIR (common on macOS, e.g. under /var/folders/...), can exceed the
	// ~104-byte sun_path limit unix domain sockets are bound by. Use a short,
	// flat MkdirTemp instead so this test's socket path stays well under that
	// limit on every platform; under systemd (the real deployment path)
	// RUNTIME_DIRECTORY is always the short /run/serverwatch, so this is
	// purely a test-environment accommodation.
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

	api := newInprocAPI(getSnap, getCfg, nil, stateDir, reload)

	stop, err := serveControlSocket(api)
	if err != nil {
		t.Fatalf("serveControlSocket: %v", err)
	}
	defer stop()

	wantPath := filepath.Join(runtimeDir, "control.sock")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("socket not created at %s: %v", wantPath, err)
	}

	client, err := control.Dial(wantPath)
	if err != nil {
		t.Fatalf("control.Dial: %v", err)
	}
	defer client.Close()

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
