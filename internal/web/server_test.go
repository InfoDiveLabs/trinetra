//go:build web

package web

import "testing"

// TestStartReturnsNoopStop exercises the Task-1 stub: Start must accept a
// Deps value and return a working (non-nil, non-panicking) stop func and a
// nil error. Later tasks (issue #58+) replace this with real assertions
// about a bound listener; for now it just pins the contract callers
// (internal/serverwatch/daemon_web.go) depend on.
func TestStartReturnsNoopStop(t *testing.T) {
	stop, err := Start(Deps{
		Enabled: false,
		Listen:  "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if stop == nil {
		t.Fatal("Start returned a nil stop func")
	}
	stop() // must not panic
}
