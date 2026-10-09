package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/control"
	"github.com/InfoDiveLabs/trinetra/internal/core"
	"github.com/InfoDiveLabs/trinetra/internal/web"
)

// fakeAPI is a minimal core.API test double (core.API has no shared one): read fields back
// one read method each.
type fakeAPI struct {
	snapshot core.DashboardView
	events   []core.DownEventView
	cfg      *config.Config

	testedChannel string
}

func (f *fakeAPI) Snapshot() (core.DashboardView, error)    { return f.snapshot, nil }
func (f *fakeAPI) Monitoring() (core.MonitoringView, error) { return core.MonitoringView{}, nil }
func (f *fakeAPI) Series(metric string, from, to int64, res core.Resolution) ([]core.SeriesPoint, error) {
	return nil, nil
}
func (f *fakeAPI) Events(from, to int64) ([]core.DownEventView, error) { return f.events, nil }
func (f *fakeAPI) ActiveAlerts() ([]core.AlertRecord, error)           { return nil, nil }
func (f *fakeAPI) AlertHistory(since int64, limit int) ([]core.AlertRecord, error) {
	return nil, nil
}
func (f *fakeAPI) Config() (*config.Config, error) { return f.cfg, nil }
func (f *fakeAPI) Doctor() (core.DoctorReport, error) {
	return core.DoctorReport{}, nil
}
func (f *fakeAPI) HostInfo() (core.HostInfoView, error) { return core.HostInfoView{}, nil }
func (f *fakeAPI) ContainerLogs(name string, lines int) (string, error) {
	return "", nil
}
func (f *fakeAPI) Version() (string, error)                                { return "v-test", nil }
func (f *fakeAPI) EnrollmentPIN(ctx context.Context) (string, bool, error) { return "", false, nil }
func (f *fakeAPI) MonitorTargets(ctx context.Context) ([]core.TargetView, error) {
	return nil, nil
}
func (f *fakeAPI) UpdateStatus() (core.UpdateStatusView, error) {
	return core.UpdateStatusView{}, nil
}
func (f *fakeAPI) UpdateCheck(ctx context.Context) (core.UpdateStatusView, error) {
	return core.UpdateStatusView{}, nil
}
func (f *fakeAPI) UpdateApply(ctx context.Context, version string) error { return nil }
func (f *fakeAPI) UpdateRollback() error                                 { return nil }
func (f *fakeAPI) ApplyConfig(c *config.Config) error                    { f.cfg = c; return nil }
func (f *fakeAPI) AckAlert(key string) error                             { return nil }
func (f *fakeAPI) UnackAlert(key string) error                           { return nil }
func (f *fakeAPI) TestChannel(name string) error                         { f.testedChannel = name; return nil }
func (f *fakeAPI) ValidateChannel(cc config.ChannelConfig) error {
	return nil
}
func (f *fakeAPI) Subscribe(ctx context.Context) (<-chan core.Event, error) {
	return nil, errors.New("not supported in fakeAPI")
}

// shortSocketPath returns a short temp socket path: t.TempDir() plus a long test
// name and macOS $TMPDIR can exceed the ~104-byte sun_path limit.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-web")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// startFakeServer serves api over a short-pathed unix socket protected by token
// and returns the socket path.
func startFakeServer(t *testing.T, api core.API, token string) (socketPath string) {
	t.Helper()
	socketPath = shortSocketPath(t)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	go control.Serve(api, ln, token)
	t.Cleanup(func() { ln.Close() })
	return socketPath
}

func envLookup(m map[string]string) func(string) string {
	return func(key string) string {
		return m[key]
	}
}

// TestResolveConnConfigDefaults pins resolution with no flags and no
// TRINETRA_CONTROL_*/SERVERWATCH_CONTROL_* env.
func TestResolveConnConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cc, err := resolveConnConfig(nil, envLookup(map[string]string{"RUNTIME_DIRECTORY": dir}))
	if err != nil {
		t.Fatalf("resolveConnConfig: %v", err)
	}
	if want := filepath.Join(dir, "control.sock"); cc.socketPath != want {
		t.Errorf("socketPath = %q, want %q", cc.socketPath, want)
	}
	if want := filepath.Join(dir, "token"); cc.tokenFile != want {
		t.Errorf("tokenFile = %q, want %q", cc.tokenFile, want)
	}
	if cc.token != "" {
		t.Errorf("token = %q, want empty (no token file written)", cc.token)
	}
	if cc.stateDir != defaultStateDir {
		t.Errorf("stateDir = %q, want %q", cc.stateDir, defaultStateDir)
	}
}

// TestResolveConnConfigReadsTokenFile: with no explicit token, the token is read
// from tokenFile with a trailing newline trimmed (see bytesTrimNewline).
func TestResolveConnConfigReadsTokenFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("abc123\n"), 0o600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}
	cc, err := resolveConnConfig(nil, envLookup(map[string]string{"RUNTIME_DIRECTORY": dir}))
	if err != nil {
		t.Fatalf("resolveConnConfig: %v", err)
	}
	if cc.token != "abc123" {
		t.Errorf("token = %q, want %q", cc.token, "abc123")
	}
}

// TestResolveConnConfigEnvOverrides: TRINETRA_CONTROL_SOCKET/TOKEN take priority
// over the RUNTIME_DIRECTORY default, and a directly set token skips tokenFile.
func TestResolveConnConfigEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "elsewhere.sock")
	cc, err := resolveConnConfig(nil, envLookup(map[string]string{
		"RUNTIME_DIRECTORY":       dir,
		"TRINETRA_CONTROL_SOCKET": sockPath,
		"TRINETRA_CONTROL_TOKEN":  "envtoken",
	}))
	if err != nil {
		t.Fatalf("resolveConnConfig: %v", err)
	}
	if cc.socketPath != sockPath {
		t.Errorf("socketPath = %q, want %q", cc.socketPath, sockPath)
	}
	if cc.token != "envtoken" {
		t.Errorf("token = %q, want %q", cc.token, "envtoken")
	}
}

// TestResolveConnConfigEnvOverrides_OldNameFallback: with TRINETRA_CONTROL_* unset, the old
// SERVERWATCH_CONTROL_* names still work for one release.
func TestResolveConnConfigEnvOverrides_OldNameFallback(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "elsewhere.sock")
	cc, err := resolveConnConfig(nil, envLookup(map[string]string{
		"RUNTIME_DIRECTORY":          dir,
		"SERVERWATCH_CONTROL_SOCKET": sockPath,
		"SERVERWATCH_CONTROL_TOKEN":  "envtoken",
	}))
	if err != nil {
		t.Fatalf("resolveConnConfig: %v", err)
	}
	if cc.socketPath != sockPath {
		t.Errorf("socketPath = %q, want %q", cc.socketPath, sockPath)
	}
	if cc.token != "envtoken" {
		t.Errorf("token = %q, want %q", cc.token, "envtoken")
	}
}

// TestResolveConnConfigFlagsOverrideEnv: explicit flags win over env and defaults.
func TestResolveConnConfigFlagsOverrideEnv(t *testing.T) {
	dir := t.TempDir()
	cc, err := resolveConnConfig(
		[]string{"-socket", "/flag/sock", "-token", "flagtoken", "-state-dir", "/flag/state"},
		envLookup(map[string]string{
			"RUNTIME_DIRECTORY":          dir,
			"SERVERWATCH_CONTROL_SOCKET": filepath.Join(dir, "env.sock"),
			"SERVERWATCH_CONTROL_TOKEN":  "envtoken",
		}),
	)
	if err != nil {
		t.Fatalf("resolveConnConfig: %v", err)
	}
	if cc.socketPath != "/flag/sock" {
		t.Errorf("socketPath = %q, want /flag/sock", cc.socketPath)
	}
	if cc.token != "flagtoken" {
		t.Errorf("token = %q, want flagtoken", cc.token)
	}
	if cc.stateDir != "/flag/state" {
		t.Errorf("stateDir = %q, want /flag/state", cc.stateDir)
	}
}

// TestBuildDepsWiresLiveDataThroughSocket: a real control.Serve over a temp socket, backed
// by a fakeAPI standing in for the daemon.
func TestBuildDepsWiresLiveDataThroughSocket(t *testing.T) {
	dir := t.TempDir()
	api := &fakeAPI{
		snapshot: core.DashboardView{TS: 1234, CPU: 42},
		events:   []core.DownEventView{{Type: "power_down", Start: 1, End: 2, DurationSec: 1}},
		cfg:      config.Default(),
	}
	api.cfg.Web.Enabled = true
	api.cfg.Web.Listen = "127.0.0.1:8099"
	sock := startFakeServer(t, api, "sekret")

	client, err := control.Dial(sock, "sekret")
	if err != nil {
		t.Fatalf("control.Dial: %v", err)
	}
	defer client.Close()

	deps := buildDeps(client, connConfig{stateDir: dir})

	if deps.API == nil {
		t.Fatal("deps.API is nil")
	}
	view, err := deps.API.Snapshot()
	if err != nil {
		t.Fatalf("deps.API.Snapshot: %v", err)
	}
	if view.CPU != 42 {
		t.Errorf("deps.API.Snapshot().CPU = %v, want 42 (from the socket-side fakeAPI, not a local stub)", view.CPU)
	}

	if deps.Snapshot == nil {
		t.Fatal("deps.Snapshot is nil")
	}
	if got := deps.Snapshot(); got.CPU != 42 {
		t.Errorf("deps.Snapshot().CPU = %v, want 42", got.CPU)
	}

	if deps.Events == nil {
		t.Fatal("deps.Events is nil")
	}
	evs, err := deps.Events.Events(0, 10)
	if err != nil {
		t.Fatalf("deps.Events.Events: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "power_down" {
		t.Errorf("deps.Events.Events = %+v, want the one power_down event from the socket-side fakeAPI", evs)
	}

	if !deps.Enabled {
		t.Error("deps.Enabled = false, want true (from the socket-side fakeAPI's Config().Web.Enabled)")
	}
	if deps.Listen != "127.0.0.1:8099" {
		t.Errorf("deps.Listen = %q, want 127.0.0.1:8099 (from the socket-side fakeAPI's Config().Web.Listen)", deps.Listen)
	}

	if err := deps.TestChannel("telegram"); err != nil {
		t.Fatalf("deps.TestChannel: %v", err)
	}
	if api.testedChannel != "telegram" {
		t.Errorf("fakeAPI.testedChannel = %q, want telegram (TestChannel call didn't cross the socket)", api.testedChannel)
	}

	// #79: the web editor must validate a channel through the daemon
	// (client.ValidateChannel) at save time, so deps.ValidateChannel must be set.
	if deps.ValidateChannel == nil {
		t.Fatal("deps.ValidateChannel is nil, want it wired to client.ValidateChannel (#79)")
	}
	if err := deps.ValidateChannel(config.ChannelConfig{Name: "phone", Type: "telegram"}, nil); err != nil {
		t.Fatalf("deps.ValidateChannel: %v", err)
	}
}

// freeLoopbackAddr picks a free "127.0.0.1:port" by binding an ephemeral listener and
// closing it (slightly racy but fine for tests).
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// TestPublicPageServesLiveSnapshotOverSocket is the one full unauthenticated HTTP path: a
// control.Serve-backed fakeAPI, turned into web.Deps by buildDeps.
func TestPublicPageServesLiveSnapshotOverSocket(t *testing.T) {
	dir := t.TempDir()
	addr := freeLoopbackAddr(t)

	cfg := config.Default()
	cfg.Web.Enabled = true
	cfg.Web.Listen = addr
	cfg.Public.Enabled = true
	cfg.Public.Panels = []string{"cpu"}

	api := &fakeAPI{
		snapshot: core.DashboardView{CPU: 77},
		cfg:      cfg,
	}
	sock := startFakeServer(t, api, "sekret")

	client, err := control.Dial(sock, "sekret")
	if err != nil {
		t.Fatalf("control.Dial: %v", err)
	}
	defer client.Close()

	deps := buildDeps(client, connConfig{stateDir: dir})

	stop, err := web.Start(deps)
	if err != nil {
		t.Fatalf("web.Start: %v", err)
	}
	defer stop()

	httpClient := &http.Client{Timeout: 5 * time.Second}
	var resp *http.Response
	// web.Start's listener bind happens synchronously inside Start, but poll briefly anyway
	// rather than assume the first dial always lands.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = httpClient.Get("http://" + addr + "/")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET http://%s/: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(string(body), "77%") {
		t.Errorf("GET / body missing the live snapshot's cpu value (77%%), the socket-side fakeAPI's data never reached the served page:\n%s", body)
	}
}
