package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerRespectsConcurrencyCap(t *testing.T) {
	var cur, peak atomic.Int32
	var done atomic.Int32
	probe := func(ctx context.Context, c Check, to time.Duration) Result {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		return Result{OK: true}
	}
	s := NewScheduler(probe, func(Check, Result) { done.Add(1) })
	s.jitter = func(time.Duration) time.Duration { return 0 }
	var checks []Check
	for i := 0; i < 48; i++ {
		checks = append(checks, Check{Name: fmt.Sprintf("c%d", i), Kind: KindTCP, Target: "x:1"})
	}
	s.Apply(checks, time.Hour, time.Second)
	defer s.Stop()
	deadline := time.Now().Add(5 * time.Second)
	for done.Load() < 48 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if done.Load() < 48 {
		t.Fatalf("only %d of 48 checks ran", done.Load())
	}
	if peak.Load() > maxInflight {
		t.Fatalf("peak in flight = %d, cap %d", peak.Load(), maxInflight)
	}
	if peak.Load() < 2 {
		t.Fatalf("peak in flight = %d, expected parallelism", peak.Load())
	}
}

func TestSchedulerUsesPerCheckIntervalAndDefaults(t *testing.T) {
	var mu sync.Mutex
	runs := map[string]int{}
	timeouts := map[string]time.Duration{}
	probe := func(ctx context.Context, c Check, to time.Duration) Result {
		mu.Lock()
		runs[c.Name]++
		timeouts[c.Name] = to
		mu.Unlock()
		return Result{OK: true}
	}
	s := NewScheduler(probe, func(Check, Result) {})
	s.jitter = func(time.Duration) time.Duration { return 0 }
	s.Apply([]Check{
		{Name: "fast", Interval: 20 * time.Millisecond, Timeout: 3 * time.Second},
		{Name: "slow"},
	}, time.Hour, 7*time.Second)
	time.Sleep(200 * time.Millisecond)
	s.Stop()
	mu.Lock()
	defer mu.Unlock()
	if runs["fast"] < 3 {
		t.Errorf("fast ran %d times", runs["fast"])
	}
	if runs["slow"] != 1 {
		t.Errorf("slow ran %d times, want 1", runs["slow"])
	}
	if timeouts["fast"] != 3*time.Second || timeouts["slow"] != 7*time.Second {
		t.Errorf("timeouts = %v", timeouts)
	}
}

func TestSchedulerApplyReplacesAndStopHalts(t *testing.T) {
	var n atomic.Int32
	probe := func(ctx context.Context, c Check, to time.Duration) Result { n.Add(1); return Result{OK: true} }
	s := NewScheduler(probe, func(Check, Result) {})
	s.jitter = func(time.Duration) time.Duration { return 0 }
	s.Apply([]Check{{Name: "a", Interval: 10 * time.Millisecond}}, time.Hour, time.Second)
	time.Sleep(60 * time.Millisecond)
	s.Apply(nil, time.Hour, time.Second)
	after := n.Load()
	time.Sleep(80 * time.Millisecond)
	if n.Load() != after {
		t.Fatalf("probe kept running after Apply(nil): %d -> %d", after, n.Load())
	}
	s.Stop()
}
