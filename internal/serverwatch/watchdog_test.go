package serverwatch

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func gate(tick, slow int64) livenessGate {
	lt := &atomic.Int64{}
	lt.Store(tick)
	ls := &atomic.Int64{}
	ls.Store(slow)
	return livenessGate{lastTick: lt, lastSlowSuccess: ls, samplerStaleSec: 30, collectorStaleSec: 300}
}

func TestLivenessGate(t *testing.T) {
	now := int64(1000)
	cases := []struct {
		name       string
		tick, slow int64
		want       bool
	}{
		{"both fresh", 990, 900, true},
		{"sampler stale", 900, 990, false},   // 100s > 30s
		{"collector stale", 990, 600, false}, // 400s > 300s
		{"both stale", 900, 600, false},
	}
	for _, c := range cases {
		if got := gate(c.tick, c.slow).alive(now); got != c.want {
			t.Errorf("%s: alive=%v want %v", c.name, got, c.want)
		}
	}
}

func TestWatchdogPeriodSecFallback(t *testing.T) {
	cases := []struct {
		name string
		usec string
		want int64
	}{
		{"unset falls back to 30", "", 30},
		{"90s watchdog -> 30 (coincides with fallback)", "90000000", 30},
		{"60s watchdog -> 20 (discriminates from fallback)", "60000000", 20},
		{"1.5s watchdog floors to 1", "1500000", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("WATCHDOG_USEC", c.usec)
			if got := watchdogPeriodSec(); got != c.want {
				t.Errorf("watchdogPeriodSec() with WATCHDOG_USEC=%q = %d, want %d", c.usec, got, c.want)
			}
		})
	}
}

func TestRunWatchdogPingsOnlyWhileAlive(t *testing.T) {
	g := gate(1000, 1000)
	var now atomic.Int64
	now.Store(1000)
	var pings atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())

	go runWatchdog(ctx, g, 1, func() int64 { return now.Load() },
		func() error { pings.Add(1); return nil })

	// Alive: expect pings to accrue.
	time.Sleep(1200 * time.Millisecond)
	if pings.Load() == 0 {
		t.Fatal("expected pings while alive")
	}
	// Make sampler stale: pings must stop advancing.
	g.lastTick.Store(0)
	before := pings.Load()
	time.Sleep(1200 * time.Millisecond)
	if pings.Load() != before {
		t.Fatalf("pings advanced while stale: %d -> %d", before, pings.Load())
	}
	cancel()
}
