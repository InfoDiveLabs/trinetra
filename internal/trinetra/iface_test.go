package trinetra

import (
	"testing"
	"time"
)

// A child (sh) that spawns a grandchild (sleep) which inherits the stdout
// pipe and outlives a naive CommandContext kill. Run must still return at
// the deadline, not block ~30s waiting on the grandchild. The deadline is the
// package-level execTimeout (configurable via exec_timeout); shorten it here
// so the test doesn't wait the generous production default.
func TestOsExecRunKillsProcessGroupOnTimeout(t *testing.T) {
	orig := execTimeout
	execTimeout = 2 * time.Second
	t.Cleanup(func() { execTimeout = orig })

	start := time.Now()
	_, err := osExec{}.Run("sh", "-c", "sleep 30 & wait")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	// Return near the 2s deadline, well before the grandchild's 30s sleep.
	if elapsed > 8*time.Second {
		t.Fatalf("Run blocked %s; expected return near the %s deadline", elapsed, execTimeout)
	}
}
