//go:build trinetra_testkeys

package trinetra

import (
	"testing"
	"time"
)

// TestE2ETimerHooksInTestkeysBuild pins the trinetra_testkeys side of the update-e2e timer
// overrides.
func TestE2ETimerHooksInTestkeysBuild(t *testing.T) {
	for _, tc := range []struct {
		env        string
		wantLoop   time.Duration
		wantHealth time.Duration
	}{
		{"", 5 * time.Minute, 90 * time.Second},
		{"5s", 5 * time.Second, 5 * time.Second},
		{"250ms", 250 * time.Millisecond, 250 * time.Millisecond},
		{"bogus", 5 * time.Minute, 90 * time.Second},
		{"0s", 5 * time.Minute, 90 * time.Second},
		{"-3s", 5 * time.Minute, 90 * time.Second},
	} {
		t.Setenv("TRINETRA_E2E_UPDATE_LOOP_INTERVAL", tc.env)
		t.Setenv("TRINETRA_E2E_HEALTH_DEADLINE", tc.env)
		if got := updateLoopEvery(); got != tc.wantLoop {
			t.Errorf("env %q: updateLoopEvery() = %v, want %v", tc.env, got, tc.wantLoop)
		}
		if got := healthDeadline(); got != tc.wantHealth {
			t.Errorf("env %q: healthDeadline() = %v, want %v", tc.env, got, tc.wantHealth)
		}
	}
}
