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

// fakeAPI is a minimal core.API test double, the same shape
// internal/control/server_test.go and internal/web/deps_api_test.go each
// keep their own copy of (core.API has no test-double package of its own to
// share one from): every read field backs exactly one read method, and the
// two write methods this test cares about record their argument.
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

// shortSocketPath returns a temp-dir socket path independent of the test
// name, mirroring internal/control/client_test.go's helper of the same
// name: t.TempDir() nests under a per-test-name directory that, combined
// with a long test name and a long $TMPDIR (common on macOS, e.g. under
// /var/folders/...), can exceed the ~104-byte sun_path limit unix domain
// sockets are bound by. os.MkdirTemp with a short, fixed prefix keeps the
// path well under that limit regardless of the calling test's name.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw-web")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

// startFakeServer serves api over a fresh, short-pathed unix socket,
// protected by token, and returns the socket path. Mirrors how
// internal/trinetra/control_socket.go wires up control.Serve, minus the
// daemon-only pieces (runtime dir discovery, token file writing) this
// test drives directly instead.
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

// TestResolveConnConfigDefaults pins the fully-default resolution path (no
// flags, no TRINETRA_CONTROL_*/SERVERWATCH_CONTROL_* env set): socket/token
// mirror internal/trinetra/control_socket.go's RUNTIME_DIRECTORY-based
// resolution, and stateDir/alertLogPath/alertStatePath fall back to
// defaultStateDir and its two well-known filenames.
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

// TestResolveConnConfigReadsTokenFile pins that, absent an explicit token
// (flag or env), resolveConnConfig reads the token from tokenFile and trims
// a trailing newline (a token typed/echoed into a file by hand often has
// one; writeTokenFile itself never writes one, see bytesTrimNewline's doc).
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

// TestResolveConnConfigEnvOverrides pins that TRINETRA_CONTROL_SOCKET/
// TRINETRA_CONTROL_TOKEN (the form Task 3's core supervisor launches this
// binary with) take priority over the mirrored RUNTIME_DIRECTORY-based
// default, and that a directly-set token skips reading tokenFile entirely.
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

// TestResolveConnConfigEnvOverrides_OldNameFallback pins the compat side:
// with TRINETRA_CONTROL_SOCKET/TOKEN unset, resolveConnConfig still honors
// the old SERVERWATCH_CONTROL_SOCKET/TOKEN names for one release, so this
// binary still works when spawned by a pre-rename core.
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

// TestResolveConnConfigFlagsOverrideEnv pins that explicit flags win over
// both env vars and the mirrored default, the documented top priority.
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

// TestBuildDepsWiresLiveDataThroughSocket is this task's core TDD case: a
// real control.Serve, over a temp unix socket, backed by a fakeAPI standing
// in for the daemon -- exactly what Task 3's supervised trinetra-web
// would dial in production. It proves buildDeps' Deps.API (what
// dashboardHandler and friends read through, see web.Deps.API's doc) and
// Deps.Events (used by the alerts page's uptime tile) both reflect data
// that only exists on the OTHER side of the socket, i.e. the wiring this
// task exists to build actually crosses the process boundary rather than
// reading some local stub.
//
// Driving web.Start (a real listener + HTTP handshake, then the dashboard's
// own auth-gated route) on top of this would mostly be re-testing
// internal/web's own server_test.go; this test instead pins the
// Deps-construction/handler-wiring seam directly against the socket
// client, per this task's own allowance for a case where driving Start
// would be heavy. TestPublicPageServesLiveSnapshotOverSocket below still
// exercises one real end-to-end HTTP path through web.Start.
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
	// (client.ValidateChannel) at save time, not skip validation entirely
	// (deps.ValidateChannel == nil) as it did before this task.
	if deps.ValidateChannel == nil {
		t.Fatal("deps.ValidateChannel is nil, want it wired to client.ValidateChannel (#79)")
	}
	if err := deps.ValidateChannel(config.ChannelConfig{Name: "phone", Type: "telegram"}, nil); err != nil {
		t.Fatalf("deps.ValidateChannel: %v", err)
	}
}

// freeLoopbackAddr picks an available "127.0.0.1:port" by binding an
// ephemeral TCP listener and immediately closing it -- the standard,
// slightly-racy-but-good-enough-for-tests trick for handing web.Start a
// concrete address a test's own http.Client can then dial (web.Start binds
// its own listener, so a "127.0.0.1:0" Deps.Listen wouldn't let this test
// discover the real port afterward).
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

// TestPublicPageServesLiveSnapshotOverSocket drives the one full,
// unauthenticated, real-HTTP path through this binary's wiring: a
// control.Serve-backed fakeAPI, dialed and turned into web.Deps by
// buildDeps (same as TestBuildDepsWiresLiveDataThroughSocket), actually
// bound and served by web.Start, then hit with a real HTTP GET. cfg.Public
// is what lets an anonymous request see snapshot-derived content without
// needing a signed-in session/passkey ceremony (see internal/web's
// rootHandler/publicPageHandler) -- the authenticated dashboard route
// (Deps.API.Snapshot, dashboardHandler) is covered at the Deps layer above
// instead of here, per this task's note that driving a full authenticated
// request through Start would be heavy.
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
	// web.Start's listener bind happens synchronously inside Start, but poll
	// briefly anyway rather than assume the first dial always lands -- cheap
	// insurance against a flaky first connection.
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

func TestUsersWorksWithDaemonStopped(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte(`{"web":{"origin":"https://ops.example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := configPath
	configPath = cfgFile
	t.Cleanup(func() { configPath = orig })
	out, err := os.CreateTemp(dir, "out")
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(string) string { return "" }
	args := []string{"-socket", filepath.Join(dir, "missing.sock"), "-state-dir", dir, "users", "invite", "--role", "admin"}
	if code := runTo(args, getenv, out, out); code != 0 {
		b, _ := os.ReadFile(out.Name())
		t.Fatalf("exit %d: %s", code, b)
	}
	b, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(b), "https://ops.example.com/enroll?token=") {
		t.Fatalf("no enroll URL from config.json: %s", b)
	}
}
