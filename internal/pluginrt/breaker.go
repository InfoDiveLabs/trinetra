package pluginrt

import (
	"sync"
	"time"
)

const (
	BreakerMaxCrashes = 5
	BreakerWindow     = 10 * time.Minute
)

// Breaker trips after BreakerMaxCrashes crashes within BreakerWindow.
type Breaker struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	now     func() time.Time
	crashes []time.Time
	tripped bool
}

// NewBreaker returns a breaker reading time from now (time.Now when nil).
func NewBreaker(now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{max: BreakerMaxCrashes, window: BreakerWindow, now: now}
}

// Crash records a crash; true exactly once, when the 5th crash within the window trips it.
func (b *Breaker) Crash() (tripped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	keep := b.crashes[:0]
	for _, c := range b.crashes {
		if t.Sub(c) < b.window {
			keep = append(keep, c)
		}
	}
	b.crashes = append(keep, t)
	if b.tripped || len(b.crashes) < b.max {
		return false
	}
	b.tripped = true
	return true
}

// Tripped reports whether the breaker has tripped.
func (b *Breaker) Tripped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tripped
}

// Reset clears recorded crashes and the tripped state.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.crashes, b.tripped = nil, false
}
