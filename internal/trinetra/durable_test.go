package trinetra

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// TestCopyFileAndPluginManifestFsyncDir is R20: every binary replace
// (install, swap, snapshot, restore) goes through copyFile, and the swap
// rewrites plugins.json; both must fsync their directory after the rename so
// a power loss never leaves a zero-length binary or manifest behind.
func TestCopyFileAndPluginManifestFsyncDir(t *testing.T) {
	dir := t.TempDir()
	prevState := stateDir
	stateDir = filepath.Join(dir, "state")
	t.Cleanup(func() { stateDir = prevState })
	var synced []string
	prev := update.SyncDir
	update.SyncDir = func(d string) error { synced = append(synced, d); return nil }
	t.Cleanup(func() { update.SyncDir = prev })

	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("BIN"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	if err := copyFile(src, filepath.Join(bin, "trinetra-web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePluginManifest(bin); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 2 || synced[0] != bin || synced[1] != stateDir {
		t.Fatalf("dir fsyncs = %v, want [%s %s]", synced, bin, stateDir)
	}
	if fi, _ := os.Stat(pluginManifestPath()); fi.Mode().Perm() != 0o600 {
		t.Fatalf("plugins.json mode %v", fi.Mode().Perm())
	}
}
