package trinetra

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/update"
)

type fakeHealth struct {
	active  bool
	version string
	ts      int64
}

func (f *fakeHealth) Active() bool { return f.active }
func (f *fakeHealth) Version() (string, error) {
	if f.version == "" {
		return "", errors.New("down")
	}
	return f.version, nil
}
func (f *fakeHealth) SampleTS() (int64, error) { return f.ts, nil }

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func guardFixture(t *testing.T, pending update.Pending) (updatePaths, *fakeClock) {
	p := testUpdatePaths(t)
	os.MkdirAll(p.previous(), 0o700)
	os.WriteFile(filepath.Join(p.previous(), "trinetra"), []byte("OLD-core"), 0o755)
	os.WriteFile(filepath.Join(p.BinDir, "trinetra"), []byte("NEW-core"), 0o755)
	update.SaveState(p.dir(), update.State{Floor: "0.4.1", Pending: &pending})
	return p, &fakeClock{t: time.Unix(1000, 0)}
}

func TestGuardCommitsWhenHealthy(t *testing.T) {
	p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
	h := &fakeHealth{active: true, version: "v0.5.0", ts: 1002}
	restarts := 0
	r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { restarts++; return nil }})
	if err != nil || r.Outcome != "committed" || restarts != 1 {
		t.Fatalf("r=%+v err=%v restarts=%d", r, err, restarts)
	}
	st, _ := update.LoadState(p.dir())
	if st.Floor != "0.5.0" || st.Pending != nil {
		t.Fatalf("state %+v", st)
	}
}

func TestGuardRollsBackOnEachFailedCondition(t *testing.T) {
	for name, h := range map[string]*fakeHealth{
		"inactive":     {active: false, version: "0.5.0", ts: 1002},
		"old version":  {active: true, version: "0.4.1", ts: 1002},
		"stale sample": {active: true, version: "0.5.0", ts: 999},
		"socket down":  {active: true, version: "", ts: 1002},
	} {
		p, clk := guardFixture(t, update.Pending{Version: "0.5.0", From: "0.4.1", Deadline: 1090, Files: []string{"trinetra"}})
		r, err := runGuard(guardDeps{paths: p, health: h, now: clk.now, sleep: clk.sleep, restart: func() error { return nil }})
		if err != nil || r.Outcome != "rolled_back" {
			t.Errorf("%s: r=%+v err=%v", name, r, err)
			continue
		}
		b, _ := os.ReadFile(filepath.Join(p.BinDir, "trinetra"))
		st, _ := update.LoadState(p.dir())
		if string(b) != "OLD-core" || st.Floor != "0.4.1" || !st.IsBad("0.5.0") || st.Pending != nil {
			t.Errorf("%s: bin=%q state=%+v", name, b, st)
		}
	}
}

func TestResumePendingOnStartLaunchesGuardOnce(t *testing.T) {
	p, _ := guardFixture(t, update.Pending{Version: "0.5.0", Deadline: 1090})
	n := 0
	if err := resumePendingOnStart(p, func() error { n++; return nil }); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	update.SaveState(p.dir(), update.State{})
	n = 0
	resumePendingOnStart(p, func() error { n++; return nil })
	if n != 0 {
		t.Fatal("guard launched with nothing pending")
	}
}
