package trinetra

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// fakeProc is a concurrency-safe fake of supervisedProc.
type fakeProc struct {
	exitCh chan error

	mu        sync.Mutex
	killCount int
}

func newFakeProc() *fakeProc {
	return &fakeProc{exitCh: make(chan error, 1)}
}

func (p *fakeProc) Wait() error { return <-p.exitCh }

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killCount++
	p.mu.Unlock()
	// Non-blocking send: Wait may already be unblocked by a prior exit, and
	// Kill must never block or panic on a second call.
	select {
	case p.exitCh <- fmt.Errorf("signal: killed"):
	default:
	}
	return nil
}

// exit simulates the child process exiting on its own with err.
func (p *fakeProc) exit(err error) {
	p.exitCh <- err
}

func (p *fakeProc) wasKilled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killCount > 0
}

// spawnCall records one call to the fake startWebProc.
type spawnCall struct {
	path string
	env  []string
}

// webTestHarness wires fake seams (startWebProc, resolveWebPlugin, supervisorSleep,
// supervisorLog) for a single test and records everything observable through them.
type webTestHarness struct {
	mu     sync.Mutex
	spawns []spawnCall
	sleeps []time.Duration
	logs   []string

	// spawnCh delivers the fakeProc created by each startWebProc call, so
	// tests can wait for a spawn to happen without ever sleeping.
	spawnCh chan *fakeProc
}

// setupWebTest installs fake seams and restores the real ones on cleanup.
func setupWebTest(t *testing.T) *webTestHarness {
	t.Helper()

	prevStartWebProc := startWebProc
	prevResolveWebPlugin := resolveWebPlugin
	prevSupervisorSleep := supervisorSleep
	prevSupervisorLog := supervisorLog
	prevTimeNow := timeNow
	t.Cleanup(func() {
		startWebProc = prevStartWebProc
		resolveWebPlugin = prevResolveWebPlugin
		supervisorSleep = prevSupervisorSleep
		supervisorLog = prevSupervisorLog
		timeNow = prevTimeNow
	})

	h := &webTestHarness{spawnCh: make(chan *fakeProc, 64)}

	startWebProc = func(path string, env []string) (supervisedProc, error) {
		h.mu.Lock()
		h.spawns = append(h.spawns, spawnCall{path: path, env: append([]string(nil), env...)})
		h.mu.Unlock()
		p := newFakeProc()
		h.spawnCh <- p
		return p, nil
	}
	resolveWebPlugin = func() (string, error) {
		return "/opt/trinetra/trinetra-web", nil
	}
	supervisorSleep = func(d time.Duration) {
		h.mu.Lock()
		h.sleeps = append(h.sleeps, d)
		h.mu.Unlock()
	}
	supervisorLog = func(format string, args ...any) {
		h.mu.Lock()
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
		h.mu.Unlock()
	}
	timeNow = time.Now

	return h
}

// nextSpawn waits for the next startWebProc call and returns its fakeProc.
func (h *webTestHarness) nextSpawn(t *testing.T) *fakeProc {
	t.Helper()
	select {
	case p := <-h.spawnCh:
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a spawn")
		return nil
	}
}

func (h *webTestHarness) spawnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.spawns)
}

func (h *webTestHarness) lastSpawn() spawnCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.spawns[len(h.spawns)-1]
}

func (h *webTestHarness) sleepDurations() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.sleeps...)
}

func (h *webTestHarness) logsContaining(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		if contains(l, substr) {
			return true
		}
	}
	return false
}

func (h *webTestHarness) logsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

// contains is a tiny substring check kept local to the test file so this
// file does not need to import "strings" just for one use.
func contains(s, substr string) bool {
	return len(substr) == 0 || indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	n, m := len(s), len(substr)
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == substr {
			return i
		}
	}
	return -1
}

// containsEnv reports whether env contains an exact "KEY=VALUE" entry.
func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// TestStartWeb_SpawnsWithVerifiedPathAndEnv: startWeb resolves the plugin path via.
func TestStartWeb_SpawnsWithVerifiedPathAndEnv(t *testing.T) {
	h := setupWebTest(t)

	stop := startWeb("/run/trinetra/control.sock", "tok123")

	proc := h.nextSpawn(t)

	call := h.lastSpawn()
	if call.path != "/opt/trinetra/trinetra-web" {
		t.Fatalf("spawn path = %q, want %q", call.path, "/opt/trinetra/trinetra-web")
	}
	if !containsEnv(call.env, "TRINETRA_CONTROL_SOCKET=/run/trinetra/control.sock") {
		t.Fatalf("spawn env missing TRINETRA_CONTROL_SOCKET, got %v", call.env)
	}
	if !containsEnv(call.env, "TRINETRA_CONTROL_TOKEN=tok123") {
		t.Fatalf("spawn env missing TRINETRA_CONTROL_TOKEN, got %v", call.env)
	}
	if !containsEnv(call.env, "SERVERWATCH_CONTROL_SOCKET=/run/trinetra/control.sock") {
		t.Fatalf("spawn env missing compat SERVERWATCH_CONTROL_SOCKET, got %v", call.env)
	}
	if !containsEnv(call.env, "SERVERWATCH_CONTROL_TOKEN=tok123") {
		t.Fatalf("spawn env missing compat SERVERWATCH_CONTROL_TOKEN, got %v", call.env)
	}

	stop()

	if !proc.wasKilled() {
		t.Fatal("stop() did not Kill the running child")
	}
}

// TestStartWeb_RestartsOnExit pins case B: when the child exits on its own
// (not via stop), the supervisor spawns it again rather than giving up.
func TestStartWeb_RestartsOnExit(t *testing.T) {
	h := setupWebTest(t)

	stop := startWeb("/run/trinetra/control.sock", "tok123")

	first := h.nextSpawn(t)
	first.exit(fmt.Errorf("boom"))

	second := h.nextSpawn(t)
	if second == first {
		t.Fatal("expected a distinct process for the restart")
	}
	if h.spawnCount() != 2 {
		t.Fatalf("spawnCount = %d, want 2", h.spawnCount())
	}

	stop()
}

// TestStartWeb_BackoffGrowsAndCaps pins case C: repeated rapid exits.
func TestStartWeb_BackoffGrowsAndCaps(t *testing.T) {
	h := setupWebTest(t)

	fixed := time.Unix(0, 0)
	timeNow = func() time.Time { return fixed }

	stop := startWeb("/run/trinetra/control.sock", "tok123")

	proc := h.nextSpawn(t)
	const wantSleeps = 7
	for i := 0; i < wantSleeps; i++ {
		proc.exit(fmt.Errorf("boom %d", i))
		proc = h.nextSpawn(t)
	}

	want := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	got := h.sleepDurations()
	if len(got) != len(want) {
		t.Fatalf("sleepDurations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sleepDurations[%d] = %v, want %v (full: %v)", i, got[i], want[i], got)
		}
	}

	stop()
}

// TestStartWeb_VerifyFailureThenRecovers pins case D: if resolveWebPlugin fails (e.g. the
// plugin fails the front-door trust check), the supervisor must NOT call startWebProc.
func TestStartWeb_VerifyFailureThenRecovers(t *testing.T) {
	h := setupWebTest(t)

	var calls int
	var mu sync.Mutex
	resolveWebPlugin = func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return "", fmt.Errorf("%w: x", errPluginVerificationFailed)
		}
		return "/opt/trinetra/trinetra-web", nil
	}

	stop := startWeb("/run/trinetra/control.sock", "tok123")

	// The only spawn that will ever arrive is the one after resolve recovers; if startWebProc
	// had been called during the failed first attempt.
	h.nextSpawn(t)
	if h.spawnCount() != 1 {
		t.Fatalf("spawnCount = %d, want 1 (startWebProc must not be called until resolve succeeds)", h.spawnCount())
	}
	// Guard against the failed first attempt's (empty) path sneaking through as this "only"
	// spawn: it must be the recovered, verified path.
	if call := h.lastSpawn(); call.path != "/opt/trinetra/trinetra-web" {
		t.Fatalf("spawn path = %q, want the recovered verified path (an unverified/empty path means a verify error reached startWebProc)", call.path)
	}
	if !h.logsContaining("refusing to start trinetra-web") {
		t.Fatalf("expected a refusal to be logged, got logs: %v", h.logsSnapshot())
	}

	stop()
}

// TestStop_DuringBackoffReturnsPromptly pins case E: if stop() is called while the loop is
// asleep in a backoff wait, it must return promptly.
func TestStop_DuringBackoffReturnsPromptly(t *testing.T) {
	h := setupWebTest(t)

	// Force the loop straight into a backoff sleep on the very first
	// iteration: resolveWebPlugin always fails, so no spawn ever happens.
	resolveWebPlugin = func() (string, error) {
		return "", fmt.Errorf("%w: x", errPluginVerificationFailed)
	}

	sleepStarted := make(chan struct{})
	var startedOnce sync.Once
	block := make(chan struct{}) // deliberately never closed
	supervisorSleep = func(d time.Duration) {
		startedOnce.Do(func() { close(sleepStarted) })
		<-block
	}

	stop := startWeb("/run/trinetra/control.sock", "tok123")

	select {
	case <-sleepStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the loop to enter its backoff sleep")
	}

	stopReturned := make(chan struct{})
	go func() {
		stop()
		close(stopReturned)
	}()

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return promptly while asleep in backoff")
	}

	if h.spawnCount() != 0 {
		t.Fatalf("spawnCount = %d, want 0 (resolve always failed, so no spawn should ever happen)", h.spawnCount())
	}
}

// TestShouldStartWeb is the unit-level guard that cmdDaemon spawns the web supervisor only
// when both the control socket is up.
func TestShouldStartWeb(t *testing.T) {
	cases := []struct {
		name     string
		socketUp bool
		enabled  bool
		want     bool
	}{
		{"socket up, web enabled", true, true, true},
		{"socket up, web disabled", true, false, false},
		{"socket down, web enabled", false, true, false},
		{"socket down, web disabled", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Web.Enabled = c.enabled
			if got := shouldStartWeb(cfg, c.socketUp); got != c.want {
				t.Errorf("shouldStartWeb(cfg{Web.Enabled:%v}, socketUp:%v) = %v, want %v", c.enabled, c.socketUp, got, c.want)
			}
		})
	}
}
