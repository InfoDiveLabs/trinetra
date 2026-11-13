package trinetra

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func fastSupervisor(t *testing.T) {
	t.Helper()
	prevSleep := supervisorSleep
	supervisorSleep = func(time.Duration) {}
	t.Cleanup(func() { supervisorSleep = prevSleep })
}

func TestSuperviseCompanionStopsWhenOnExitFalse(t *testing.T) {
	fastSupervisor(t)
	var mu sync.Mutex
	starts, exits := 0, 0
	spec := companionSpec{
		Name:    "x",
		Resolve: func() (string, error) { return "/bin/x", nil },
		Start: func(string, []string) (supervisedProc, error) {
			mu.Lock()
			starts++
			mu.Unlock()
			p := newFakeProc()
			p.exit(errors.New("boom"))
			return p, nil
		},
		OnExit: func(error, time.Duration) bool {
			mu.Lock()
			defer mu.Unlock()
			exits++
			return exits < 2
		},
		Logf: func(string, ...any) {},
	}
	done := make(chan struct{})
	go superviseCompanion(spec, make(chan struct{}), done)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	if starts != 2 {
		t.Fatalf("starts = %d, want 2", starts)
	}
}

func TestSuperviseCompanionResolvesBeforeEverySpawn(t *testing.T) {
	fastSupervisor(t)
	var resolves, starts int
	exits := 0
	spec := companionSpec{
		Name:    "x",
		Resolve: func() (string, error) { resolves++; return "/bin/x", nil },
		Start: func(string, []string) (supervisedProc, error) {
			starts++
			p := newFakeProc()
			p.exit(nil)
			return p, nil
		},
		OnExit: func(error, time.Duration) bool { exits++; return exits < 3 },
		Logf:   func(string, ...any) {},
	}
	done := make(chan struct{})
	go superviseCompanion(spec, make(chan struct{}), done)
	<-done
	if resolves != 3 || starts != 3 {
		t.Fatalf("resolves=%d starts=%d, want 3 each", resolves, starts)
	}
}

func TestSuperviseCompanionRefusesWhenResolveFails(t *testing.T) {
	fastSupervisor(t)
	calls := 0
	stop := make(chan struct{})
	spec := companionSpec{
		Name: "x",
		Resolve: func() (string, error) {
			calls++
			if calls == 3 {
				close(stop)
			}
			return "", errors.New("tampered")
		},
		Start: func(string, []string) (supervisedProc, error) {
			t.Error("must not start after a failed resolve")
			return nil, errors.New("no")
		},
		Logf: func(string, ...any) {},
	}
	done := make(chan struct{})
	go superviseCompanion(spec, stop, done)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestStartCompanionStopKillsChild(t *testing.T) {
	proc := newFakeProc()
	started := make(chan struct{})
	stop := startCompanion(companionSpec{
		Name:    "x",
		Resolve: func() (string, error) { return "/bin/x", nil },
		Start:   func(string, []string) (supervisedProc, error) { close(started); return proc, nil },
		Logf:    func(string, ...any) {},
	})
	<-started
	stop()
	if !proc.wasKilled() {
		t.Fatal("stop did not kill the child")
	}
}

func TestFirstPartyCompanionNames(t *testing.T) {
	if got := firstPartyCompanionNames(); !reflect.DeepEqual(got, []string{"ctl", "web"}) {
		t.Fatalf("got %v", got)
	}
}

func TestWritePluginManifestUsesCompanionNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"trinetra-ctl", "trinetra-web", "trinetra-foo"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := stateDir
	stateDir = t.TempDir()
	t.Cleanup(func() { stateDir = prev })
	if err := writePluginManifest(dir); err != nil {
		t.Fatal(err)
	}
	m, err := loadPluginManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m["ctl"] == "" || m["web"] == "" || m["foo"] != "" {
		t.Fatalf("manifest = %v", m)
	}
}
