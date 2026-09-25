package trinetra

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// fakeLaunch records the arguments it was called with and returns whatever
// err is set, standing in for launchPlugin (which would otherwise
// syscall.Exec and never return -- unusable directly from a test).
type fakeLaunch struct {
	calledName       string
	calledArgs       []string
	calledSocketPath string
	calledToken      string
	err              error
}

func (f *fakeLaunch) fn(name string, args []string, socketPath, token string) error {
	f.calledName = name
	f.calledArgs = args
	f.calledSocketPath = socketPath
	f.calledToken = token
	return f.err
}

// withFakeLaunch overrides launchPluginFn for the duration of the test and
// restores it on cleanup, since it is a shared package var.
func withFakeLaunch(t *testing.T, f *fakeLaunch) {
	t.Helper()
	prev := launchPluginFn
	launchPluginFn = f.fn
	t.Cleanup(func() { launchPluginFn = prev })
}

// captureStderr swaps the package stderr var for a buffer for the duration
// of the test and restores it on cleanup.
func captureStderr(t *testing.T) *bytes.Buffer {
	t.Helper()
	prev := stderr
	var buf bytes.Buffer
	stderr = &buf
	t.Cleanup(func() { stderr = prev })
	return &buf
}

func TestCmdFrontDoor_PassesCorrectArgsToLauncher(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", tmpdir)
	if err := os.WriteFile(filepath.Join(tmpdir, "token"), []byte("abc123"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &fakeLaunch{}
	withFakeLaunch(t, f)
	captureStderr(t)

	code := cmdFrontDoor("cli", "ctl", []string{"--foo"})
	if code != 0 {
		t.Fatalf("cmdFrontDoor exit = %d, want 0", code)
	}
	if f.calledName != "ctl" {
		t.Errorf("name = %q, want %q", f.calledName, "ctl")
	}
	if !reflect.DeepEqual(f.calledArgs, []string{"--foo"}) {
		t.Errorf("args = %v, want %v", f.calledArgs, []string{"--foo"})
	}
	wantSocket := filepath.Join(tmpdir, "control.sock")
	if f.calledSocketPath != wantSocket {
		t.Errorf("socketPath = %q, want %q", f.calledSocketPath, wantSocket)
	}
	if f.calledToken != "abc123" {
		t.Errorf("token = %q, want %q", f.calledToken, "abc123")
	}
}

func TestCmdFrontDoor_NotInstalled_Cli(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", tmpdir)

	f := &fakeLaunch{err: fmt.Errorf("%w: x", errPluginNotInstalled)}
	withFakeLaunch(t, f)
	buf := captureStderr(t)

	code := cmdFrontDoor("cli", "ctl", nil)
	if code != 1 {
		t.Fatalf("cmdFrontDoor exit = %d, want 1", code)
	}
	got := buf.String()
	for _, want := range []string{"serverwatch-ctl", "not installed", "serverwatch install", "Download", "releases"} {
		if !containsFold(got, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, got)
		}
	}
}

func TestCmdFrontDoor_NotInstalled_Web(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", tmpdir)

	f := &fakeLaunch{err: fmt.Errorf("%w: x", errPluginNotInstalled)}
	withFakeLaunch(t, f)
	buf := captureStderr(t)

	code := cmdFrontDoor("web", "web", nil)
	if code != 1 {
		t.Fatalf("cmdFrontDoor exit = %d, want 1", code)
	}
	got := buf.String()
	for _, want := range []string{"serverwatch-web", "not installed", "serverwatch install", "go build -o /usr/local/bin/serverwatch-web ./cmd/serverwatch-web"} {
		if !containsFold(got, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, got)
		}
	}
	if containsFold(got, "-tags web") {
		t.Errorf("stderr should not mention -tags web (the build tag no longer exists); got:\n%s", got)
	}
}

func TestCmdFrontDoor_VerificationFailed(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", tmpdir)

	f := &fakeLaunch{err: fmt.Errorf("%w: checksum mismatch", errPluginVerificationFailed)}
	withFakeLaunch(t, f)
	buf := captureStderr(t)

	code := cmdFrontDoor("cli", "ctl", nil)
	if code != 1 {
		t.Fatalf("cmdFrontDoor exit = %d, want 1", code)
	}
	got := buf.String()
	for _, want := range []string{"refusing", "tampering", "checksum mismatch"} {
		if !containsFold(got, want) {
			t.Errorf("stderr missing %q; got:\n%s", want, got)
		}
	}
}

func TestCmdFrontDoor_TokenFileUnreadable_EmptyTokenStillExecs(t *testing.T) {
	tmpdir := t.TempDir()
	t.Setenv("RUNTIME_DIRECTORY", tmpdir)
	// Deliberately do not create a token file.

	f := &fakeLaunch{}
	withFakeLaunch(t, f)
	captureStderr(t)

	code := cmdFrontDoor("cli", "ctl", nil)
	if code != 0 {
		t.Fatalf("cmdFrontDoor exit = %d, want 0", code)
	}
	if f.calledToken != "" {
		t.Errorf("token = %q, want empty", f.calledToken)
	}
}

// containsFold reports whether s contains substr, ignoring case.
func containsFold(s, substr string) bool {
	return bytes.Contains(
		bytes.ToLower([]byte(s)),
		bytes.ToLower([]byte(substr)),
	)
}
