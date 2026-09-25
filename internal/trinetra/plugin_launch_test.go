package trinetra

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newTestPlugin creates a temp file at dir/name with content and mode 0o755
// (owner rwx, group/other read+exec but NOT write, so it passes the
// group/world-writable check by default). Tests that want to violate a
// specific invariant chmod it further after this call.
func newTestPlugin(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("write test plugin: %v", err)
	}
	return path
}

func TestVerifyPlugin_Success(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("pretend plugin binary"))

	sum, err := sha256File(path)
	if err != nil {
		t.Fatalf("sha256File: %v", err)
	}
	manifest := map[string]string{"ctl": sum}

	if err := verifyPlugin(path, os.Getuid(), manifest, "ctl"); err != nil {
		t.Fatalf("verifyPlugin: want nil, got %v", err)
	}
}

func TestVerifyPlugin_GroupWorldWritableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("x"))
	// Add the world-write bit: violates mode.Perm()&0o022 == 0.
	if err := os.Chmod(path, 0o757); err != nil {
		t.Fatal(err)
	}

	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	err = verifyPlugin(path, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for world-writable file, got %v", err)
	}
}

func TestVerifyPlugin_GroupWritableFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("x"))
	// Add the group-write bit only.
	if err := os.Chmod(path, 0o775); err != nil {
		t.Fatal(err)
	}

	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	err = verifyPlugin(path, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for group-writable file, got %v", err)
	}
}

func TestVerifyPlugin_GroupWorldWritableParentDir(t *testing.T) {
	dir := t.TempDir()
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("x"))
	// Loosen the parent directory instead of the file.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) // let t.TempDir() clean up

	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	err = verifyPlugin(path, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for world-writable parent dir, got %v", err)
	}
}

func TestVerifyPlugin_ChecksumMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("original content"))

	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	// Tamper with the file AFTER recording its checksum in the manifest.
	if err := os.WriteFile(path, []byte("tampered content"), 0o755); err != nil {
		t.Fatal(err)
	}

	err = verifyPlugin(path, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for checksum mismatch, got %v", err)
	}
}

func TestVerifyPlugin_MissingManifestEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("x"))

	// Manifest exists but has no entry for "ctl": must fail closed, not
	// pass because there's simply nothing to compare against.
	manifest := map[string]string{}

	err := verifyPlugin(path, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for missing manifest entry, got %v", err)
	}
}

func TestVerifyPlugin_WrongOwner(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := newTestPlugin(t, dir, "trinetra-ctl", []byte("x"))

	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	// The file is actually owned by os.Getuid(), but tell verifyPlugin to
	// expect some other, definitely-different non-root uid.
	wrongUID := os.Getuid() + 1
	err = verifyPlugin(path, wrongUID, manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for wrong owner, got %v", err)
	}
}

func TestVerifyPlugin_NotRegularFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named like a plugin is not a regular file.
	subdir := filepath.Join(dir, "trinetra-ctl")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := verifyPlugin(subdir, os.Getuid(), map[string]string{"ctl": "whatever"}, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for a directory, got %v", err)
	}
}

func TestVerifyPlugin_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real := newTestPlugin(t, dir, "trinetra-ctl-real", []byte("x"))
	link := filepath.Join(dir, "trinetra-ctl")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	sum, err := sha256File(real)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{"ctl": sum}

	// verifyPlugin must Lstat, not Stat: called directly on a symlink (as
	// opposed to a path pluginPath already resolved) it must refuse, not
	// silently follow it.
	err = verifyPlugin(link, os.Getuid(), manifest, "ctl")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for a symlink, got %v", err)
	}
}

func TestVerifyPlugin_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trinetra-ctl")

	err := verifyPlugin(path, os.Getuid(), map[string]string{"ctl": "whatever"}, "ctl")
	if !errors.Is(err, errPluginNotInstalled) {
		t.Fatalf("want errPluginNotInstalled for missing file, got %v", err)
	}
	if errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("missing file must NOT also match errPluginVerificationFailed (callers branch on this): %v", err)
	}
}

// TestPluginPath_NeverConsultsPath plants a plugin-shaped binary on $PATH
// and asserts pluginPath ignores it entirely: it must resolve strictly
// relative to the core binary's own directory (os.Executable()), which is
// the PATH-hijack defense from the threat model.
func TestPluginPath_NeverConsultsPath(t *testing.T) {
	pathDir := t.TempDir()
	fake := filepath.Join(pathDir, "trinetra-pathtrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)

	_, err := pluginPath("pathtrap")
	if !errors.Is(err, errPluginNotInstalled) {
		t.Fatalf("pluginPath must never consult $PATH; got err=%v, want errPluginNotInstalled", err)
	}
}

func TestPluginPath_NotInstalled(t *testing.T) {
	_, err := pluginPath("definitely-does-not-exist-xyz123")
	if !errors.Is(err, errPluginNotInstalled) {
		t.Fatalf("want errPluginNotInstalled, got %v", err)
	}
}

// TestPluginPath_ResolvesNextToCoreBinary and its symlink counterpart write
// a synthetic plugin file directly next to the running test binary (what
// os.Executable() reports during `go test`), which is normally a writable
// temp directory. If the sandbox this happens to run in doesn't allow that,
// skip rather than fail: the behavior is still covered indirectly by
// TestResolveAndVerifyPlugin_Success below via resolveAndVerifyPlugin.
func TestPluginPath_ResolvesNextToCoreBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	target := filepath.Join(dir, "trinetra-synthtest-target")
	if err := os.WriteFile(target, []byte("x"), 0o755); err != nil {
		t.Skipf("cannot write next to test binary at %s (%v); skipping", dir, err)
	}
	t.Cleanup(func() { os.Remove(target) })

	got, err := pluginPath("synthtest-target")
	if err != nil {
		t.Fatalf("pluginPath: %v", err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Clean(want)
	if got != want {
		t.Fatalf("pluginPath = %q, want %q", got, want)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("pluginPath returned a non-absolute path: %q", got)
	}
}

func TestPluginPath_ResolvesSymlink(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	real := filepath.Join(dir, "trinetra-synthtest-real")
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Skipf("cannot write next to test binary at %s (%v); skipping", dir, err)
	}
	t.Cleanup(func() { os.Remove(real) })

	link := filepath.Join(dir, "trinetra-synthtest-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot symlink next to test binary at %s (%v); skipping", dir, err)
	}
	t.Cleanup(func() { os.Remove(link) })

	got, err := pluginPath("synthtest-link")
	if err != nil {
		t.Fatalf("pluginPath: %v", err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	want = filepath.Clean(want)
	if got != want {
		t.Fatalf("pluginPath through symlink = %q, want %q (the real target)", got, want)
	}
}

func TestLoadPluginManifest(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	want := map[string]string{"ctl": "abc123", "web": "def456"}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginManifestPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadPluginManifest()
	if err != nil {
		t.Fatalf("loadPluginManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadPluginManifest = %v, want %v", got, want)
	}
}

func TestLoadPluginManifest_Missing(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir() // empty: no plugins.json written
	t.Cleanup(func() { stateDir = prevStateDir })

	_, err := loadPluginManifest()
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("a missing manifest must be a verification FAILURE, not a silent pass; got %v", err)
	}
}

func TestLoadPluginManifest_Corrupt(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	if err := os.WriteFile(pluginManifestPath(), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := loadPluginManifest()
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for a corrupt manifest, got %v", err)
	}
}

// TestResolveAndVerifyPlugin_Success exercises the full resolve+verify path
// launchPlugin runs before it would call syscall.Exec (which can't itself
// be unit tested since it replaces the process on success). Everything up
// to, but not including, the exec is covered here.
func TestResolveAndVerifyPlugin_Success(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	pluginFile := filepath.Join(dir, "trinetra-synthtest-rav")
	content := []byte("pretend trinetra-rav plugin binary")
	if err := os.WriteFile(pluginFile, content, 0o755); err != nil {
		t.Skipf("cannot write next to test binary at %s (%v); skipping", dir, err)
	}
	t.Cleanup(func() { os.Remove(pluginFile) })

	sum, err := sha256File(pluginFile)
	if err != nil {
		t.Fatal(err)
	}

	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	manifest := map[string]string{"synthtest-rav": sum}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginManifestPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	path, err := resolveAndVerifyPlugin("synthtest-rav")
	if err != nil {
		t.Fatalf("resolveAndVerifyPlugin: %v", err)
	}
	want, err := filepath.EvalSymlinks(pluginFile)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Clean(want) {
		t.Fatalf("resolveAndVerifyPlugin path = %q, want %q", path, want)
	}
}

// TestResolveAndVerifyPlugin_TamperedBinary tampers the plugin AFTER the
// manifest is written (mirroring a swap attack) and confirms
// resolveAndVerifyPlugin refuses -- i.e. that launchPlugin would never
// reach syscall.Exec in this case.
func TestResolveAndVerifyPlugin_TamperedBinary(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(exe)
	pluginFile := filepath.Join(dir, "trinetra-synthtest-tamper")
	if err := os.WriteFile(pluginFile, []byte("original"), 0o755); err != nil {
		t.Skipf("cannot write next to test binary at %s (%v); skipping", dir, err)
	}
	t.Cleanup(func() { os.Remove(pluginFile) })

	sum, err := sha256File(pluginFile)
	if err != nil {
		t.Fatal(err)
	}

	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	manifest := map[string]string{"synthtest-tamper": sum}
	b, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pluginManifestPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Swap the binary's contents after the manifest was recorded.
	if err := os.WriteFile(pluginFile, []byte("attacker-controlled payload"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = resolveAndVerifyPlugin("synthtest-tamper")
	if !errors.Is(err, errPluginVerificationFailed) {
		t.Fatalf("want errPluginVerificationFailed for a tampered binary, got %v", err)
	}
}

func TestResolveAndVerifyPlugin_NotInstalled(t *testing.T) {
	prevStateDir := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prevStateDir })

	_, err := resolveAndVerifyPlugin("definitely-does-not-exist-xyz123")
	if !errors.Is(err, errPluginNotInstalled) {
		t.Fatalf("want errPluginNotInstalled, got %v", err)
	}
}

func TestSha256File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello")
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Fatalf("sha256File(hello) = %s, want %s", got, want)
	}
}
