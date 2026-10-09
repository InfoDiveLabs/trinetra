// coreapi_inproc_update_test.go: inprocAPI's control-socket UpdateApply and UpdateRollback
// must return quickly.
package trinetra

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/config"
	"github.com/InfoDiveLabs/trinetra/internal/update"
)

// blockingReleaseSource is an update.Source whose ReleaseAsset blocks until release is
// closed (or ctx is cancelled), then always fails.
type blockingReleaseSource struct {
	release chan struct{}
}

func (s blockingReleaseSource) ReleaseAsset(ctx context.Context, version, name string) (io.ReadCloser, error) {
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, errors.New("blockingReleaseSource: no such asset")
}

func (s blockingReleaseSource) ChannelAsset(ctx context.Context, name string) (io.ReadCloser, error) {
	return nil, update.ErrNoChannel
}

// newBlockingUpdateAPI builds an *inprocAPI wired (via the newUpdaterFn test seam,
// coreapi_inproc.go) to a blockingReleaseSource and isolated temp paths.
func newBlockingUpdateAPI(t *testing.T, release chan struct{}) *inprocAPI {
	t.Helper()
	root := t.TempDir()
	paths := updatePaths{BinDir: filepath.Join(root, "bin"), StateDir: filepath.Join(root, "state")}
	src := blockingReleaseSource{release: release}
	return &inprocAPI{
		getCfg: func() *config.Config { return config.Default() },
		newUpdaterFn: func(c *config.Config) updater {
			return updater{
				paths:       paths,
				keys:        update.KeySet{},
				x:           osExec{},
				now:         time.Now,
				arch:        "amd64",
				running:     update.Version{},
				launchGuard: func() error { return nil },
				src:         src,
			}
		},
	}
}

// TestInprocUpdateApplyReturnsBeforeSlowSourceFinishes pins that UpdateApply returns almost
// immediately, well before a permanently blocked Source finishes.
func TestInprocUpdateApplyReturnsBeforeSlowSourceFinishes(t *testing.T) {
	release := make(chan struct{}) // never closed in this test
	api := newBlockingUpdateAPI(t, release)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- api.UpdateApply(context.Background(), "9.9.9") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateApply returned an error from its fast preflight: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UpdateApply did not return within 2s; it must not block on the (permanently blocked) Source")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("UpdateApply took %s to return, want well under the blocked Source's lifetime", elapsed)
	}

	st, err := api.UpdateStatus()
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if !st.InProgress {
		t.Error("UpdateStatus.InProgress = false immediately after UpdateApply returned, want true (the goroutine is still blocked on the Source)")
	}
}

// TestInprocUpdateApplyRefusesConcurrentSecondCall pins that a second UpdateApply while the
// first is blocked on the Source is refused with a fixed error, not queued.
func TestInprocUpdateApplyRefusesConcurrentSecondCall(t *testing.T) {
	release := make(chan struct{})
	api := newBlockingUpdateAPI(t, release)
	// Release the blocked goroutine and wait for it before the TempDirs that
	// newBlockingUpdateAPI registered are removed (cleanups run LIFO, so this one runs first).
	t.Cleanup(func() {
		close(release)
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if v, err := api.UpdateStatus(); err == nil && !v.InProgress {
				return
			}
		}
	})

	if err := api.UpdateApply(context.Background(), "9.9.9"); err != nil {
		t.Fatalf("first UpdateApply: %v", err)
	}

	err := api.UpdateApply(context.Background(), "9.9.9")
	if !errors.Is(err, errUpdateAlreadyRunning) {
		t.Fatalf("second concurrent UpdateApply = %v, want errUpdateAlreadyRunning", err)
	}

	// Same refusal for a concurrent Rollback: the in-flight slot is shared across
	// apply/rollback, not per-method.
	if err := api.UpdateRollback(); !errors.Is(err, errUpdateAlreadyRunning) {
		t.Fatalf("concurrent UpdateRollback while an apply is in flight = %v, want errUpdateAlreadyRunning", err)
	}
}

// TestInprocUpdateApplyClearsInProgressAndRecordsLastError pins the goroutine's completion
// side: once the Source is released (and therefore apply's FetchRelease call fails).
func TestInprocUpdateApplyClearsInProgressAndRecordsLastError(t *testing.T) {
	release := make(chan struct{})
	api := newBlockingUpdateAPI(t, release)

	if err := api.UpdateApply(context.Background(), "9.9.9"); err != nil {
		t.Fatalf("UpdateApply: %v", err)
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	var st struct {
		inProgress bool
		lastErr    string
	}
	for time.Now().Before(deadline) {
		v, err := api.UpdateStatus()
		if err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
		st.inProgress, st.lastErr = v.InProgress, v.LastError
		if !st.inProgress {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st.inProgress {
		t.Fatal("InProgress never went back to false after the Source was released")
	}
	if st.lastErr == "" {
		t.Error("LastError is empty after a failed apply, want the failure recorded")
	}

	// The slot is free again: a fresh call must be accepted (not refused as still-running).
	release2 := make(chan struct{})
	api.newUpdaterFn = func(c *config.Config) updater {
		return updater{
			paths:       updatePaths{BinDir: t.TempDir(), StateDir: t.TempDir()},
			keys:        update.KeySet{},
			x:           osExec{},
			now:         time.Now,
			arch:        "amd64",
			running:     update.Version{},
			launchGuard: func() error { return nil },
			src:         blockingReleaseSource{release: release2},
		}
	}
	if err := api.UpdateApply(context.Background(), "9.9.9"); err != nil {
		t.Fatalf("UpdateApply after the previous one finished: %v", err)
	}

	// Let the second apply finish before returning: its goroutine writes update state into
	// this test's TempDirs, and t.TempDir's cleanup fails.
	close(release2)
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if v, err := api.UpdateStatus(); err == nil && !v.InProgress {
			return
		}
	}
	t.Fatal("second apply never finished after its Source was released")
}
