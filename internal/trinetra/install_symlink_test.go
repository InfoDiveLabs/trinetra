package trinetra

import (
	"os"
	"path/filepath"
	"testing"
)

// #82: install must also expose the binary on a directory that sudo's secure_path includes
// (e.g. /usr/bin), so `sudo trinetra ...` resolves on distros.

func TestLinkOnPathCreatesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "local", "trinetra")
	link := filepath.Join(dir, "bin", "trinetra")
	mustWrite(t, target, "BINARY")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := linkOnPath(target, link); err != nil {
		t.Fatalf("linkOnPath: %v", err)
	}
	dest, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("link is not a symlink: %v", err)
	}
	if dest != target {
		t.Errorf("symlink points at %q, want %q", dest, target)
	}
}

func TestLinkOnPathIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "trinetra")
	link := filepath.Join(dir, "link")
	mustWrite(t, target, "BINARY")

	if err := linkOnPath(target, link); err != nil {
		t.Fatalf("first linkOnPath: %v", err)
	}
	if err := linkOnPath(target, link); err != nil {
		t.Fatalf("second linkOnPath (idempotent) errored: %v", err)
	}
	if dest, _ := os.Readlink(link); dest != target {
		t.Errorf("symlink points at %q, want %q", dest, target)
	}
}

func TestLinkOnPathDoesNotClobberExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "trinetra")
	link := filepath.Join(dir, "existing")
	mustWrite(t, target, "BINARY")
	mustWrite(t, link, "DISTRO-BINARY") // a real file already at the link path

	if err := linkOnPath(target, link); err != nil {
		t.Fatalf("linkOnPath: %v", err)
	}
	got, _ := os.ReadFile(link)
	if string(got) != "DISTRO-BINARY" {
		t.Errorf("existing file was clobbered: content = %q", got)
	}
}

func TestUnlinkOnPathRemovesOurSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "trinetra")
	link := filepath.Join(dir, "link")
	mustWrite(t, target, "BINARY")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	unlinkOnPath(target, link)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("our symlink should have been removed, Lstat err = %v", err)
	}
}

func TestUnlinkOnPathLeavesForeignFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "trinetra")
	link := filepath.Join(dir, "real")
	mustWrite(t, target, "BINARY")
	mustWrite(t, link, "NOT-OURS") // a real file, not our symlink

	unlinkOnPath(target, link)
	if got, err := os.ReadFile(link); err != nil || string(got) != "NOT-OURS" {
		t.Errorf("a non-symlink at the link path must be left intact; got %q err %v", got, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
