package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverwatch/internal/control"
	"serverwatch/internal/core"
)

func TestResolveSocketPathPrecedence(t *testing.T) {
	t.Setenv("SERVERWATCH_CONTROL_SOCKET", "/env/control.sock")
	t.Setenv("RUNTIME_DIRECTORY", "/rt")

	if got := resolveSocketPath("/flag/control.sock"); got != "/flag/control.sock" {
		t.Errorf("flag override = %q, want /flag/control.sock", got)
	}
	if got := resolveSocketPath(""); got != "/env/control.sock" {
		t.Errorf("env override = %q, want /env/control.sock", got)
	}

	os.Unsetenv("SERVERWATCH_CONTROL_SOCKET")
	if got := resolveSocketPath(""); got != "/rt/control.sock" {
		t.Errorf("RUNTIME_DIRECTORY = %q, want /rt/control.sock", got)
	}

	os.Unsetenv("RUNTIME_DIRECTORY")
	if got := resolveSocketPath(""); got != defaultRuntimeDir+"/control.sock" {
		t.Errorf("default = %q, want %s/control.sock", got, defaultRuntimeDir)
	}
}

func TestResolveTokenPathDefaultsToSocketSibling(t *testing.T) {
	os.Unsetenv("SERVERWATCH_CONTROL_TOKEN")
	got := resolveTokenPath("", "/some/dir/control.sock")
	if want := "/some/dir/token"; got != want {
		t.Errorf("token path = %q, want %q", got, want)
	}
}

func TestResolveTokenReadsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveToken(path, "")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("token = %q, want deadbeef (trimmed)", got)
	}
}

func TestResolveTokenMissingFileIsEmpty(t *testing.T) {
	os.Unsetenv("SERVERWATCH_CONTROL_TOKEN")
	got, err := resolveToken(filepath.Join(t.TempDir(), "nope"), "")
	if err != nil {
		t.Fatalf("missing token file should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}

// TestEndToEndStatusOverRealSocket stands up a real control.Serve over a temp
// unix socket + token file and drives realMain through the full path (resolve
// -> Dial -> run status), proving the served snapshot reaches the printed
// output over the wire.
func TestEndToEndStatusOverRealSocket(t *testing.T) {
	// chdir into the temp dir and use a relative socket name: unix socket
	// paths are capped (104 bytes on darwin) and t.TempDir()'s absolute path
	// alone can exceed that, so binding relative to cwd keeps sun_path short.
	dir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prevWD)

	const sockPath = "control.sock"
	const tokenPath = "token"
	const token = "s3cr3ttoken"
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	api := &fakeAPI{snapshot: core.DashboardView{
		Online: true,
		CPU:    77.7,
		Cores:  4,
	}}
	go control.Serve(api, ln, token)

	// Make sure defaults do not leak in from a developer's real environment.
	os.Unsetenv("SERVERWATCH_CONTROL_SOCKET")
	os.Unsetenv("SERVERWATCH_CONTROL_TOKEN")

	var out, errOut bytes.Buffer
	code := realMain([]string{"--socket", sockPath, "--token", tokenPath, "status"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("realMain status = %d, want 0\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "77.7") || !strings.Contains(out.String(), "online") {
		t.Errorf("status output missing served values:\n%s", out.String())
	}
}
