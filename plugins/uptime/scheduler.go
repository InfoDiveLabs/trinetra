package main

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

const (
	maxInflight = 16
	maxJitter   = 5 * time.Second
)

// Scheduler runs one goroutine per check with at most maxInflight probes at once.
type Scheduler struct {
	probe    func(ctx context.Context, c Check, timeout time.Duration) Result
	onResult func(Check, Result)
	jitter   func(max time.Duration) time.Duration
	sem      chan struct{}

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewScheduler returns a stopped scheduler.
func NewScheduler(probe func(context.Context, Check, time.Duration) Result, onResult func(Check, Result)) *Scheduler {
	return &Scheduler{
		probe: probe, onResult: onResult,
		jitter: func(max time.Duration) time.Duration { return time.Duration(rand.Int63n(int64(max) + 1)) },
		sem:    make(chan struct{}, maxInflight),
	}
}

// Apply stops the running checks and starts checks, using the defaults for zero intervals and timeouts.
func (s *Scheduler) Apply(checks []Check, defInterval, defTimeout time.Duration) {
	s.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	for _, c := range checks {
		iv, to := c.Interval, c.Timeout
		if iv <= 0 {
			iv = defInterval
		}
		if to <= 0 {
			to = defTimeout
		}
		s.wg.Add(1)
		go s.loop(ctx, c, iv, to)
	}
}

// Stop halts every check and waits for in-flight probes to return.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, c Check, interval, timeout time.Duration) {
	defer s.wg.Done()
	if !sleep(ctx, s.jitter(min(interval, maxJitter))) {
		return
	}
	for {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		res := s.probe(ctx, c, timeout)
		<-s.sem
		if ctx.Err() != nil {
			return
		}
		s.onResult(c, res)
		if !sleep(ctx, interval) {
			return
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
