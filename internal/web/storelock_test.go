package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestLockStoreExcludesOtherProcesses holds the lock here while a child
// process tries to take it; the child must block until we release.
func TestLockStoreExcludesOtherProcesses(t *testing.T) {
	if p := os.Getenv("TRINETRA_LOCK_CHILD"); p != "" {
		lockStore(p)()
		return
	}
	path := filepath.Join(t.TempDir(), "users.json")
	unlock := lockStore(path)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockStoreExcludesOtherProcesses$")
	cmd.Env = append(os.Environ(), "TRINETRA_LOCK_CHILD="+path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		unlock()
		t.Fatal("child took the lock while we held it")
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child never got the lock after release")
	}
}

func TestTokenIssueSurvivesConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	a, b := newTokenStore(dir), newTokenStore(dir)
	done := make(chan string, 40)
	for i := 0; i < 20; i++ {
		go func() { done <- a.Issue(RoleViewer, time.Hour) }()
		go func() { done <- b.Issue(RoleAdmin, time.Hour) }()
	}
	for i := 0; i < 40; i++ {
		if <-done == "" {
			t.Fatal("Issue failed")
		}
	}
	toks, err := a.List()
	if err != nil || len(toks) != 40 {
		t.Fatalf("tokens = %d (%v), want 40", len(toks), err)
	}
}

func TestLockStoreLeavesNoFileForRelativePath(t *testing.T) {
	lockStore("relative-store.json")()
	if _, err := os.Stat("relative-store.json.lock"); err == nil {
		os.Remove("relative-store.json.lock")
		t.Fatal("lock file created next to the package for a relative store path")
	}
}
