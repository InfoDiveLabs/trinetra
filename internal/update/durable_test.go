package update

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSaveStateSurfacesDirSyncError is R20: the directory fsync after the rename is what
// makes the new state.json entry durable.
func TestSaveStateSurfacesDirSyncError(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("dir fsync failed")
	prev := SyncDir
	SyncDir = func(string) error { return boom }
	t.Cleanup(func() { SyncDir = prev })
	if err := SaveState(dir, State{Floor: "0.5.0"}); !errors.Is(err, boom) {
		t.Fatalf("SaveState = %v, want the directory fsync error", err)
	}
}

// TestCopyFileDurable is R20: CopyFile writes a random same-directory temp file, fsyncs it,
// renames it over dst and fsyncs dst's directory.
func TestCopyFileDurable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "sub", "dst")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(src, []byte("NEW"), 0o600)
	os.WriteFile(dst, []byte("OLD"), 0o644)
	var synced []string
	prev := SyncDir
	SyncDir = func(d string) error { synced = append(synced, d); return nil }
	t.Cleanup(func() { SyncDir = prev })

	if err := CopyFile(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "NEW" {
		t.Fatalf("dst = %q", b)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if len(synced) != 1 || synced[0] != filepath.Dir(dst) {
		t.Fatalf("dir syncs = %v, want [%s]", synced, filepath.Dir(dst))
	}
	ents, _ := os.ReadDir(filepath.Dir(dst))
	if len(ents) != 1 {
		t.Fatalf("temp files left behind: %v", ents)
	}
}

// TestWriteFileAtomicDirSyncErrorSurfaces: a failed directory fsync after
// the rename is returned, not swallowed.
func TestWriteFileAtomicDirSyncErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("boom")
	prev := SyncDir
	SyncDir = func(string) error { return boom }
	t.Cleanup(func() { SyncDir = prev })
	if err := WriteFileAtomic(filepath.Join(dir, "f"), []byte("x"), 0o600); !errors.Is(err, boom) {
		t.Fatalf("WriteFileAtomic = %v", err)
	}
}
