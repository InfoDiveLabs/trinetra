package trinetra

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriteFileAtomicConcurrentWritersSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.json")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := writeFileAtomic(path, []byte(fmt.Sprintf(`{"n":%d}`, i)), 0o600); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writeFileAtomic: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v perm %v", err, st.Mode().Perm())
	}
	left, _ := filepath.Glob(path + ".*.tmp")
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

// TestWriteFileAtomicSyncedConcurrentWritersSamePath is the fsync'd variant of
// TestWriteFileAtomicConcurrentWritersSamePath.
func TestWriteFileAtomicSyncedConcurrentWritersSamePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silences.json")
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := writeFileAtomicSynced(path, []byte(fmt.Sprintf(`{"n":%d}`, i)), 0o600); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writeFileAtomicSynced: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v perm %v", err, st.Mode().Perm())
	}
	left, _ := filepath.Glob(path + ".*.tmp")
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
