package serverwatch

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeProc is a concurrency-safe fake of supervisedProc. Wait blocks until
// the test calls exit (simulating the child process exiting on its own) or
// Kill is called (simulating the supervisor killing it on stop); either one
// unblocks Wait exactly once.
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

// webTestHarness wires fake seams (startWebProc, resolveWebPlugin,
// supervisorSleep, supervisorLog) for a single test and records everything
// observable through them, all guarded by one mutex so it is safe under
// -race with the supervisor goroutine running concurrently.
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
// The default resolveWebPlugin resolves to a fixed verified path; the
// default supervisorSleep is instant (records the requested duration and
// returns immediately) so tests never wait on real backoff.
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
		return "/opt/serverwatch/serverwatch-web", nil
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
// It never sleeps to wait: it blocks on the harness's spawnCh, which the
// fake startWebProc feeds synchronously, and only times out (failing the
// test) if a spawn that should happen never does.
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

// TestStartWeb_SpawnsWithVerifiedPathAndEnv pins case A from the brief: on
// startWeb, the supervisor resolves the plugin path via resolveWebPlugin and
// spawns it via startWebProc with the control socket and token passed as the
// SERVERWATCH_CONTROL_SOCKET / SERVERWATCH_CONTROL_TOKEN env vars. Calling
// the returned stop func kills the running child and blocks until the
// supervisor loop has actually exited.
func TestStartWeb_SpawnsWithVerifiedPathAndEnv(t *testing.T) {
	h := setupWebTest(t)

	stop := startWeb("/run/serverwatch/control.sock", "tok123")

	proc := h.nextSpawn(t)

	call := h.lastSpawn()
	if call.path != "/opt/serverwatch/serverwatch-web" {
		t.Fatalf("spawn path = %q, want %q", call.path, "/opt/serverwatch/serverwatch-web")
	}
	if !containsEnv(call.env, "SERVERWATCH_CONTROL_SOCKET=/run/serverwatch/control.sock") {
		t.Fatalf("spawn env missing SERVERWATCH_CONTROL_SOCKET, got %v", call.env)
	}
	if !containsEnv(call.env, "SERVERWATCH_CONTROL_TOKEN=tok123") {
		t.Fatalf("spawn env missing SERVERWATCH_CONTROL_TOKEN, got %v", call.env)
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

	stop := startWeb("/run/serverwatch/control.sock", "tok123")

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

// TestStartWeb_BackoffGrowsAndCaps pins case C: repeated rapid exits (each
// looking short-lived because timeNow is frozen, so the "healthy child"
// reset never fires) make the recorded backoff sleep durations double from
// webBackoffMin, capped at webBackoffMax.
func TestStartWeb_BackoffGrowsAndCaps(t *testing.T) {
	h := setupWebTest(t)

	fixed := time.Unix(0, 0)
	timeNow = func() time.Time { return fixed }

	stop := startWeb("/run/serverwatch/control.sock", "tok123")

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

// TestStartWeb_VerifyFailureThenRecovers pins case D: if resolveWebPlugin
// fails (e.g. the plugin fails the front-door trust check), the supervisor
// must NOT call startWebProc, must log a refusal, and must keep retrying so
// it recovers once resolveWebPlugin starts succeeding again.
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
		return "/opt/serverwatch/serverwatch-web", nil
	}

	stop := startWeb("/run/serverwatch/control.sock", "tok123")

	// The only spawn that will ever arrive is the one after resolve
	// recovers; if startWebProc had been called during the failed first
	// attempt, spawnCount would already be 2 by the time this one arrives.
	h.nextSpawn(t)
	if h.spawnCount() != 1 {
		t.Fatalf("spawnCount = %d, want 1 (startWebProc must not be called until resolve succeeds)", h.spawnCount())
	}
	if !h.logsContaining("refusing to start serverwatch-web") {
		t.Fatalf("expected a refusal to be logged, got logs: %v", h.logsSnapshot())
	}

	stop()
}

// TestStop_DuringBackoffReturnsPromptly pins case E: if stop() is called
// while the loop is asleep in a backoff wait, it must return promptly
// (without waiting for the sleep to finish) and no further spawn must
// occur.
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

	stop := startWeb("/run/serverwatch/control.sock", "tok123")

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
