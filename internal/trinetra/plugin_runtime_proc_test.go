package trinetra

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
	"github.com/InfoDiveLabs/trinetra/internal/pluginrt"
)

func pluginTestRecord(t *testing.T, body string) (plugin.Installed, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "trinetra-plugin-demo")
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	rec := plugin.Installed{ID: "demo", Version: "1.0.0", Manifest: plugin.Manifest{
		ID: "demo", Name: "Demo", Version: "1.0.0",
		Binaries: []plugin.Binary{{OS: runtime.GOOS, Arch: runtime.GOARCH, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}},
	}}
	return rec, bin
}

type alertSink struct {
	mu sync.Mutex
	as []Alert
}

func (s *alertSink) add(a Alert) { s.mu.Lock(); s.as = append(s.as, a); s.mu.Unlock() }
func (s *alertSink) all() []Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Alert(nil), s.as...)
}

func stubPluginStartProc(t *testing.T, f func(rec plugin.Installed, bin, token string) (supervisedProc, error)) {
	t.Helper()
	prev := pluginStartProc
	pluginStartProc = f
	t.Cleanup(func() { pluginStartProc = prev })
}

func TestPluginCompanionCrashLoopRaisesOneInfoAlert(t *testing.T) {
	fastSupervisor(t)
	rec, bin := pluginTestRecord(t, "binary")
	var mu sync.Mutex
	starts := 0
	stubPluginStartProc(t, func(plugin.Installed, string, string) (supervisedProc, error) {
		mu.Lock()
		starts++
		mu.Unlock()
		p := newFakeProc()
		p.exit(errors.New("boom"))
		return p, nil
	})
	sink := &alertSink{}
	failed := make(chan string, 4)
	clock := time.Unix(1_700_000_000, 0)
	var cmu sync.Mutex
	d := &pluginDeps{deliverAlert: sink.add, logf: func(string, ...any) {}, now: func() time.Time {
		cmu.Lock()
		defer cmu.Unlock()
		clock = clock.Add(10 * time.Second)
		return clock
	}}
	sup := newPluginSupervisor(d, func(id, reason string) { failed <- id + ":" + reason })
	if err := sup.Start(rec, bin, "tok"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-failed:
		if !strings.HasPrefix(got, "demo:crash loop") {
			t.Fatalf("failed = %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("breaker did not trip")
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	n := starts
	mu.Unlock()
	if n != 5 {
		t.Fatalf("starts = %d, want 5 (no 6th start)", n)
	}
	as := sink.all()
	if len(as) != 1 || as[0].Severity != SevInfo || as[0].Key != "plugin.demo.crashloop" || as[0].Kind != "fire" || as[0].Source != "plugin" {
		t.Fatalf("alerts = %+v", as)
	}
	if running, restarts := sup.Running("demo"); running || restarts != 5 {
		t.Fatalf("running=%v restarts=%d", running, restarts)
	}
	_ = sup.Stop("demo")
	_ = sup.Stop("demo")
}

func TestPluginCompanionRestartsAfterCrash(t *testing.T) {
	fastSupervisor(t)
	rec, bin := pluginTestRecord(t, "binary")
	var mu sync.Mutex
	var procs []*fakeProc
	stubPluginStartProc(t, func(plugin.Installed, string, string) (supervisedProc, error) {
		mu.Lock()
		defer mu.Unlock()
		p := newFakeProc()
		if len(procs) == 0 {
			p.exit(errors.New("crash"))
		}
		procs = append(procs, p)
		return p, nil
	})
	sup := newPluginSupervisor(&pluginDeps{logf: func(string, ...any) {}}, func(string, string) { t.Error("unexpected failure") })
	_ = sup.Start(rec, bin, "tok")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(procs)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	n := len(procs)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("procs = %d", n)
	}
	if running, restarts := sup.Running("demo"); !running || restarts != 1 {
		t.Fatalf("running=%v restarts=%d", running, restarts)
	}
	_ = sup.Stop("demo")
	if running, _ := sup.Running("demo"); running {
		t.Fatal("still running after Stop")
	}
}

func TestPluginCompanionRefusesTamperedBinary(t *testing.T) {
	fastSupervisor(t)
	rec, bin := pluginTestRecord(t, "binary")
	if err := os.WriteFile(bin, []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	stubPluginStartProc(t, func(plugin.Installed, string, string) (supervisedProc, error) {
		started <- struct{}{}
		return newFakeProc(), nil
	})
	var mu sync.Mutex
	var logs []string
	sup := newPluginSupervisor(&pluginDeps{logf: func(f string, a ...any) {
		mu.Lock()
		logs = append(logs, f)
		mu.Unlock()
	}}, nil)
	_ = sup.Start(rec, bin, "tok")
	time.Sleep(150 * time.Millisecond)
	_ = sup.Stop("demo")
	select {
	case <-started:
		t.Fatal("tampered binary was started")
	default:
	}
	mu.Lock()
	defer mu.Unlock()
	if len(logs) == 0 {
		t.Fatal("refusal not logged")
	}
}

func TestPluginRuntimeUnitProcWaitReturnsOnFailed(t *testing.T) {
	var calls [][]string
	state := []string{"ActiveState=active\nResult=success\n", "ActiveState=failed\nResult=exit-code\n"}
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if args[0] == "show" {
			s := state[0]
			if len(state) > 1 {
				state = state[1:]
			}
			return []byte(s), nil
		}
		return nil, nil
	}}
	p, err := startUnitProc(x, "demo")
	if err != nil {
		t.Fatal(err)
	}
	p.sleep = func(time.Duration) {}
	if err := p.Wait(); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("Wait = %v", err)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range calls {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"systemctl start trinetra-plugin@demo.service", "systemctl show -p ActiveState,Result trinetra-plugin@demo.service", "systemctl stop trinetra-plugin@demo.service"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in\n%s", want, joined)
		}
	}
}

func TestPluginRuntimeUnitProcWaitReturnsNilOnCleanStop(t *testing.T) {
	x := fakeExec{fn: func(name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nResult=success\n"), nil
	}}
	p, _ := startUnitProc(x, "demo")
	p.sleep = func(time.Duration) {}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPluginRuntimeStartProcPicksSystemdWhenPresent(t *testing.T) {
	prevSys, prevExec, prevDeps, prevW, prevRoot := hasSystemd, pluginExecutor, currentPluginDeps, writePluginUnitFn, pluginIsRoot
	t.Cleanup(func() {
		hasSystemd, pluginExecutor, currentPluginDeps, writePluginUnitFn, pluginIsRoot = prevSys, prevExec, prevDeps, prevW, prevRoot
	})
	hasSystemd = func() bool { return true }
	pluginIsRoot = func() bool { return true }
	rx := &recExec{}
	pluginExecutor = rx
	wrote := false
	writePluginUnitFn = func(Exec, plugin.Installed, string, string, string) error { wrote = true; return nil }
	currentPluginDeps = &pluginDeps{stateDir: t.TempDir(), hostDir: "/run/trinetra-plugins"}
	rec, bin := pluginTestRecord(t, "binary")
	if _, err := defaultPluginStartProc(rec, bin, "tok"); err != nil {
		t.Fatal(err)
	}
	if !wrote || !rx.ran("systemctl start trinetra-plugin@demo.service") {
		t.Fatalf("wrote=%v calls=%v", wrote, rx.calls)
	}
}

func TestPluginRuntimeStartProcFallsBackToExecWithoutSystemd(t *testing.T) {
	prevSys, prevDeps, prevRoot, prevPrep := hasSystemd, currentPluginDeps, pluginIsRoot, pluginPrepareRuntimeDir
	t.Cleanup(func() {
		hasSystemd, currentPluginDeps, pluginIsRoot, pluginPrepareRuntimeDir = prevSys, prevDeps, prevRoot, prevPrep
	})
	pluginPrepareRuntimeDir = func(string, bool) error { return nil }
	hasSystemd = func() bool { return false }
	pluginIsRoot = func() bool { return false }
	currentPluginDeps = &pluginDeps{stateDir: t.TempDir(), hostDir: "/run/trinetra-plugins"}
	rec, bin := pluginTestRecord(t, "#!/bin/sh\nexit 0\n")
	p, err := defaultPluginStartProc(rec, bin, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestPluginCompanionSpawnFailuresTripBreakerWithReason(t *testing.T) {
	fastSupervisor(t)
	rec, bin := pluginTestRecord(t, "binary")
	stubPluginStartProc(t, func(plugin.Installed, string, string) (supervisedProc, error) {
		return nil, errors.New("user: unknown user trinetra-plugin")
	})
	failed := make(chan string, 4)
	clock := time.Unix(1_700_000_000, 0)
	var cmu sync.Mutex
	d := &pluginDeps{logf: func(string, ...any) {}, now: func() time.Time {
		cmu.Lock()
		defer cmu.Unlock()
		clock = clock.Add(10 * time.Second)
		return clock
	}}
	sup := newPluginSupervisor(d, func(id, reason string) { failed <- reason })
	_ = sup.Start(rec, bin, "tok")
	if running, _ := sup.Running("demo"); running {
		t.Fatal("running before any process started")
	}
	select {
	case got := <-failed:
		if !strings.Contains(got, "unknown user trinetra-plugin") {
			t.Fatalf("reason %q lacks the spawn error", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spawn failures never tripped the breaker")
	}
	if running, restarts := sup.Running("demo"); running || restarts != 5 {
		t.Fatalf("running=%v restarts=%d", running, restarts)
	}
	_ = sup.Stop("demo")
}

func TestPluginCompanionTamperedBinaryTripsBreaker(t *testing.T) {
	fastSupervisor(t)
	rec, bin := pluginTestRecord(t, "binary")
	if err := os.WriteFile(bin, []byte("evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubPluginStartProc(t, func(plugin.Installed, string, string) (supervisedProc, error) { return newFakeProc(), nil })
	failed := make(chan string, 4)
	clock := time.Unix(1_700_000_000, 0)
	var cmu sync.Mutex
	d := &pluginDeps{logf: func(string, ...any) {}, now: func() time.Time {
		cmu.Lock()
		defer cmu.Unlock()
		clock = clock.Add(10 * time.Second)
		return clock
	}}
	sup := newPluginSupervisor(d, func(id, reason string) { failed <- reason })
	_ = sup.Start(rec, bin, "tok")
	select {
	case got := <-failed:
		if !strings.Contains(got, "checksum mismatch") {
			t.Fatalf("reason %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tampered binary never tripped the breaker")
	}
	_ = sup.Stop("demo")
}

func TestPrepareRuntimeDirLetsPluginUsersTraverseTheRoot(t *testing.T) {
	prev := pluginrt.SocketRoot
	t.Cleanup(func() { pluginrt.SocketRoot = prev })
	pluginrt.SocketRoot = filepath.Join(t.TempDir(), "trinetra-plugin")
	if err := prepareRuntimeDir("demo", false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(pluginrt.SocketRoot)
	if err != nil || st.Mode().Perm() != 0o711 {
		t.Fatalf("socket root mode %v, %v; want 0711", st.Mode().Perm(), err)
	}
	st, err = os.Stat(filepath.Join(pluginrt.SocketRoot, "demo"))
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("plugin dir mode %v, %v; want 0700", st.Mode().Perm(), err)
	}
}
