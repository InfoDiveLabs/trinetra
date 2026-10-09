package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
)

func TestResolveSocketPathPrecedence(t *testing.T) {
	t.Setenv("TRINETRA_CONTROL_SOCKET", "/trinetra-env/control.sock")
	t.Setenv("SERVERWATCH_CONTROL_SOCKET", "/env/control.sock")
	t.Setenv("RUNTIME_DIRECTORY", "/rt")

	if got := resolveSocketPath("/flag/control.sock"); got != "/flag/control.sock" {
		t.Errorf("flag override = %q, want /flag/control.sock", got)
	}
	if got := resolveSocketPath(""); got != "/trinetra-env/control.sock" {
		t.Errorf("TRINETRA_CONTROL_SOCKET override = %q, want /trinetra-env/control.sock", got)
	}

	// With TRINETRA_CONTROL_SOCKET unset, the old (compat) name still works.
	os.Unsetenv("TRINETRA_CONTROL_SOCKET")
	if got := resolveSocketPath(""); got != "/env/control.sock" {
		t.Errorf("SERVERWATCH_CONTROL_SOCKET fallback = %q, want /env/control.sock", got)
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

func TestResolveTokenFileDefaultsToSocketSibling(t *testing.T) {
	got := resolveTokenFile("", "/some/dir/control.sock")
	if want := "/some/dir/token"; got != want {
		t.Errorf("token file = %q, want %q", got, want)
	}
}

// TestResolveTokenEnvIsValueNotPath pins the front-door/supervisor contract:
// TRINETRA_CONTROL_TOKEN.
func TestResolveTokenEnvIsValueNotPath(t *testing.T) {
	t.Setenv("TRINETRA_CONTROL_TOKEN", "plaintok-not-a-path")
	got, err := resolveToken("", "/run/trinetra/control.sock")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if got != "plaintok-not-a-path" {
		t.Errorf("token = %q, want the env value used verbatim", got)
	}
}

// TestResolveTokenEnvFallsBackToOldName pins the compat side: with TRINETRA_CONTROL_TOKEN
// unset, resolveToken still honors the old SERVERWATCH_CONTROL_TOKEN name for one release.
func TestResolveTokenEnvFallsBackToOldName(t *testing.T) {
	os.Unsetenv("TRINETRA_CONTROL_TOKEN")
	t.Setenv("SERVERWATCH_CONTROL_TOKEN", "old-name-tok")
	got, err := resolveToken("", "/run/trinetra/control.sock")
	if err != nil {
		t.Fatalf("resolveToken: %v", err)
	}
	if got != "old-name-tok" {
		t.Errorf("token = %q, want the compat env value used verbatim", got)
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
	os.Unsetenv("TRINETRA_CONTROL_TOKEN")
	os.Unsetenv("SERVERWATCH_CONTROL_TOKEN")
	got, err := resolveToken(filepath.Join(t.TempDir(), "nope"), "")
	if err != nil {
		t.Fatalf("missing token file should not error, got %v", err)
	}
	if got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}

// TestEndToEndStatusOverRealSocket stands up a real control.Serve over a temp unix socket +
// token file and drives realMain through the full path (resolve -> Dial -> run status).
func TestEndToEndStatusOverRealSocket(t *testing.T) {
	// chdir into the temp dir and use a relative socket name: unix socket paths are capped.
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
	os.Unsetenv("TRINETRA_CONTROL_SOCKET")
	os.Unsetenv("TRINETRA_CONTROL_TOKEN")
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

func TestRealMainWaitsForStartingDaemon(t *testing.T) {
	dir := t.TempDir()
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prevWD)
	os.Unsetenv("TRINETRA_CONTROL_SOCKET")
	os.Unsetenv("TRINETRA_CONTROL_TOKEN")
	os.Unsetenv("SERVERWATCH_CONTROL_SOCKET")
	os.Unsetenv("SERVERWATCH_CONTROL_TOKEN")

	const sockPath = "control.sock"
	const token = "fresh-launch-token"
	api := &fakeAPI{snapshot: core.DashboardView{Online: true, CPU: 12.5, Cores: 2}}
	go func() {
		time.Sleep(400 * time.Millisecond)
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Errorf("listen: %v", err)
			return
		}
		t.Cleanup(func() { ln.Close() })
		if err := os.WriteFile("token", []byte(token), 0o600); err != nil {
			t.Error(err)
		}
		go control.Serve(api, ln, token)
	}()

	var out, errOut bytes.Buffer
	if code := realMain([]string{"--socket", sockPath, "status"}, &out, &errOut); code != 0 {
		t.Fatalf("realMain status = %d, want 0\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "waiting for the trinetra daemon") {
		t.Errorf("stderr lacks the waiting notice:\n%s", errOut.String())
	}
	if !strings.Contains(out.String(), "12.5") {
		t.Errorf("status output missing served values:\n%s", out.String())
	}
}
