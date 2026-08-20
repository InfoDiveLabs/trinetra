// Package web: rate_limit.go bounds how fast an UNAUTHENTICATED client can
// drive the ceremony-store write path. /enroll/begin and /login/begin each do
// a full-file read-modify-write of ceremonies.json under one shared lock
// (session.go); without a cap an anonymous client can hammer that lock and
// stall new ceremonies (#95). A small in-memory fixed-window limiter, applied
// to just those two routes, caps the begins per client without touching the
// authenticated session path.
package web

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// beginRateMax and beginRateWindow are the default cap: at most beginRateMax
// ceremony begins per beginRateWindow per client key. Generous for a real user
// (a WebAuthn enroll/login is a handful of requests) yet a hard ceiling on an
// abusive client. Deliberately not a config key: this is an internal safety
// limit, not an operator tuning knob.
const (
	beginRateMax    = 15
	beginRateWindow = 10 * time.Second
)

// rateLimiter is a per-key fixed-window counter. It is safe for concurrent use.
type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time // injectable for tests; time.Now in production
	hits   map[string]*hitWindow
	lastGC time.Time
}

type hitWindow struct {
	count int
	start time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		max:    max,
		window: window,
		now:    time.Now,
		hits:   make(map[string]*hitWindow),
	}
}

// allow records a hit for key and reports whether it is within the limit. The
// window is fixed: the first hit starts the window, and once max hits land
// inside it every further hit is rejected until the window rolls over.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	rl.gcLocked(now)

	w := rl.hits[key]
	if w == nil || now.Sub(w.start) >= rl.window {
		rl.hits[key] = &hitWindow{count: 1, start: now}
		return true
	}
	if w.count >= rl.max {
		return false
	}
	w.count++
	return true
}

// gcLocked drops expired windows so the map cannot grow without bound under a
// spray of distinct client keys. Runs at most once per window.
func (rl *rateLimiter) gcLocked(now time.Time) {
	if now.Sub(rl.lastGC) < rl.window {
		return
	}
	rl.lastGC = now
	for k, w := range rl.hits {
		if now.Sub(w.start) >= rl.window {
			delete(rl.hits, k)
		}
	}
}

// clientKey identifies the caller for rate-limiting: the TCP peer host from
// RemoteAddr. It deliberately does NOT trust X-Forwarded-For, which an
// attacker could vary per request to dodge the limit. Behind a reverse proxy
// this makes the limit act as a shared ceiling for all clients of that proxy,
// which is the safe direction for a defense-in-depth cap.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimitBegin wraps an unauthenticated ceremony-begin handler: it rejects
// with 429 (and a Retry-After) once a client exceeds rl, otherwise passes
// through to next.
func rateLimitBegin(rl *rateLimiter, next http.HandlerFunc) http.HandlerFunc {
	retryAfter := strconv.Itoa(int(rl.window.Seconds()))
	return func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(clientKey(r)) {
			w.Header().Set("Retry-After", retryAfter)
			http.Error(w, "too many requests: slow down and retry", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
