package update

import (
	"os"
	"path/filepath"
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
