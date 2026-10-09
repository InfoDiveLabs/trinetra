package update

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStateRoundTripAndFloor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "update")
	s, err := LoadState(dir)
	if err != nil || s.Floor != "" {
		t.Fatalf("empty load: %+v %v", s, err)
	}
	v := func(x string) Version { y, _ := ParseVersion(x); return y }
	s.RaiseFloor(v("0.5.0"))
	s.RaiseFloor(v("0.4.9")) // must not lower
	s.Pending = &Pending{Version: "0.5.1", From: "0.5.0", Deadline: 42, Files: []string{"trinetra"}}
	s.Bad = []string{"0.5.2"}
	if err := SaveState(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil || got.Floor != "0.5.0" || got.Pending == nil || got.Pending.Version != "0.5.1" || !got.IsBad("0.5.2") {
		t.Fatalf("reload: %+v %v", got, err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", fi.Mode())
	}
}

func TestSaveStateConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = SaveState(dir, State{Floor: "0.5.0"}) }()
	}
	wg.Wait()
	if _, err := LoadState(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadStateCorruptFailsClosed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600)
	if _, err := LoadState(dir); err == nil {
		t.Fatal("corrupt state accepted; a corrupt floor must never read as 'no floor'")
	}
}

// TestFloorVersionTruthTable pins the four cases from #138: no persisted floor with an
// unknown running version means no lower bound at all.
func TestFloorVersionTruthTable(t *testing.T) {
	v := func(x string) Version {
		y, err := ParseVersion(x)
		if err != nil {
			t.Fatalf("bad test version %q: %v", x, err)
		}
		return y
	}

	t.Run("no floor, unknown running -> no floor", func(t *testing.T) {
		got, hasFloor, err := State{}.FloorVersion(Version{})
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if hasFloor {
			t.Fatalf("hasFloor = true (floor %v), want false", got)
		}
	})

	t.Run("no floor, running 0.5.0 -> floor 0.5.0", func(t *testing.T) {
		got, hasFloor, err := State{}.FloorVersion(v("0.5.0"))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !hasFloor || got != v("0.5.0") {
			t.Fatalf("floor = %v hasFloor=%v, want 0.5.0/true", got, hasFloor)
		}
	})

	t.Run("valid floor 0.5.0, running 0.4.0 -> 0.5.0", func(t *testing.T) {
		got, hasFloor, err := State{Floor: "0.5.0"}.FloorVersion(v("0.4.0"))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !hasFloor || got != v("0.5.0") {
			t.Fatalf("floor = %v hasFloor=%v, want 0.5.0/true", got, hasFloor)
		}
	})

	t.Run("valid floor 0.4.0, running 0.5.0 -> 0.5.0 (running wins)", func(t *testing.T) {
		got, hasFloor, err := State{Floor: "0.4.0"}.FloorVersion(v("0.5.0"))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !hasFloor || got != v("0.5.0") {
			t.Fatalf("floor = %v hasFloor=%v, want 0.5.0/true", got, hasFloor)
		}
	})

	t.Run("unparsable floor -> error, fails closed", func(t *testing.T) {
		_, _, err := State{Floor: "not-a-version"}.FloorVersion(v("0.4.0"))
		if err == nil {
			t.Fatal("unparsable persisted floor accepted as no-floor; must fail closed")
		}
		if !strings.Contains(err.Error(), `"not-a-version"`) {
			t.Fatalf("err = %v, want it to name the bad floor value", err)
		}
	})
}

func TestAvailableOver(t *testing.T) {
	v := func(s string) Version {
		x, err := ParseVersion(s)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	for _, tc := range []struct {
		avail, running, want string
	}{
		{"0.6.0-beta.1", "0.6.0-beta.1", ""},
		{"0.6.0-beta.1", "0.6.0", ""},
		{"0.6.0-beta.2", "0.6.0-beta.1", "0.6.0-beta.2"},
		{"0.6.0", "0.5.0", "0.6.0"},
		{"", "0.5.0", ""},
		{"garbage", "0.5.0", ""},
	} {
		if got := (State{Available: tc.avail}).AvailableOver(v(tc.running)); got != tc.want {
			t.Errorf("Available %q over running %s = %q, want %q", tc.avail, tc.running, got, tc.want)
		}
	}
}

func TestRecordResultQueuesEveryOutcome(t *testing.T) {
	var s State
	s.RecordResult(Result{Version: "0.5.0", From: "0.4.1", Outcome: "committed", At: 1})
	s.RecordResult(Result{Version: "0.5.1", From: "0.5.0", Outcome: "rolled_back", At: 2})
	if s.Last == nil || s.Last.Version != "0.5.1" {
		t.Fatalf("Last = %+v, want the newest outcome", s.Last)
	}
	if len(s.Unnotified) != 2 || s.Unnotified[0].Version != "0.5.0" || s.Unnotified[1].Version != "0.5.1" {
		t.Fatalf("Unnotified = %+v, want both outcomes oldest first", s.Unnotified)
	}
	for i := 0; i < 3*maxUnnotified; i++ {
		s.RecordResult(Result{Version: "0.6.0", Outcome: "committed", At: int64(10 + i)})
	}
	if len(s.Unnotified) != maxUnnotified {
		t.Fatalf("queue length = %d, want capped at %d", len(s.Unnotified), maxUnnotified)
	}
	if got := s.Unnotified[len(s.Unnotified)-1].At; got != int64(10+3*maxUnnotified-1) {
		t.Fatalf("newest queued At = %d, want the latest outcome kept", got)
	}
}
