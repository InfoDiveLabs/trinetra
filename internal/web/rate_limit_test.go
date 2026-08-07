package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterAllowsUpToMaxThenBlocks(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	rl := newRateLimiter(3, 10*time.Second)
	rl.now = func() time.Time { return base }

	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("hit %d denied, want allowed (under limit)", i+1)
		}
	}
	if rl.allow("1.2.3.4") {
		t.Fatal("4th hit allowed, want denied (over limit)")
	}
}

func TestRateLimiterResetsAfterWindow(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	rl := newRateLimiter(2, 10*time.Second)
	rl.now = func() time.Time { return now }

	if !rl.allow("k") || !rl.allow("k") {
		t.Fatal("first two hits should be allowed")
	}
	if rl.allow("k") {
		t.Fatal("third hit within window should be denied")
	}
	// Roll past the window: the counter resets.
	now = now.Add(10 * time.Second)
	if !rl.allow("k") {
		t.Fatal("hit after the window rolled over should be allowed")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	rl := newRateLimiter(1, 10*time.Second)
	rl.now = func() time.Time { return base }

	if !rl.allow("a") {
		t.Fatal("first hit for a denied")
	}
	if rl.allow("a") {
		t.Fatal("second hit for a should be denied")
	}
	if !rl.allow("b") {
		t.Fatal("b must have its own budget, independent of a")
	}
}

// TestRateLimitBeginHandler proves the middleware returns 429 after the limit
// and passes through under it, and that a different client is unaffected.
func TestRateLimitBeginHandler(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	rl := newRateLimiter(2, 10*time.Second)
	rl.now = func() time.Time { return base }

	var served int
	h := rateLimitBegin(rl, func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})

	do := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/enroll/begin", nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code
	}

	if c := do("10.0.0.9:5555"); c != http.StatusOK {
		t.Fatalf("hit 1 = %d, want 200", c)
	}
	if c := do("10.0.0.9:5556"); c != http.StatusOK { // same host, different port -> same key
		t.Fatalf("hit 2 = %d, want 200", c)
	}
	if c := do("10.0.0.9:5557"); c != http.StatusTooManyRequests {
		t.Fatalf("hit 3 = %d, want 429", c)
	}
	if served != 2 {
		t.Errorf("underlying handler served %d times, want 2 (the 429 must not reach it)", served)
	}
	// A different client host is unaffected by the first's exhaustion.
	if c := do("10.0.0.99:6000"); c != http.StatusOK {
		t.Fatalf("different client = %d, want 200", c)
	}
}
