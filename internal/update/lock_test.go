package update

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestTryLockIsExclusive is R16: the apply/guard locks are non-blocking
// flocks; a second holder is refused until the first releases.
func TestTryLockIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "apply.lock")
	unlock, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first TryLock: ok=%v err=%v", ok, err)
	}
	if _, ok2, err := TryLock(path); err != nil || ok2 {
		t.Fatalf("second TryLock while held: ok=%v err=%v", ok2, err)
	}
	unlock()
	unlock2, ok3, err := TryLock(path)
	if err != nil || !ok3 {
		t.Fatalf("TryLock after release: ok=%v err=%v", ok3, err)
	}
	unlock2()
}

// TestWithStateSerializesReadModifyWrite is R16: every LoadState-modify-
// SaveState runs under the state lock, so concurrent writers never lose
// each other's changes.
func TestWithStateSerializesReadModifyWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "update")
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := WithState(dir, func(s *State) error {
				s.Bad = append(s.Bad, fmt.Sprintf("0.0.%d", i))
				return nil
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	st, err := LoadState(dir)
	if err != nil || len(st.Bad) != 24 {
		t.Fatalf("lost updates: %d of 24 (%v)", len(st.Bad), err)
	}
}

// TestWithStateSkipsSaveOnError: fn's error aborts without writing.
func TestWithStateSkipsSaveOnError(t *testing.T) {
	dir := t.TempDir()
	SaveState(dir, State{Floor: "0.1.0"})
	boom := fmt.Errorf("boom")
	if err := WithState(dir, func(s *State) error { s.Floor = "9.9.9"; return boom }); err != boom {
		t.Fatalf("err = %v", err)
	}
	if st, _ := LoadState(dir); st.Floor != "0.1.0" {
		t.Fatalf("state written despite error: %+v", st)
	}
}
