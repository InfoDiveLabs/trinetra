package serverwatch

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// livenessGate decides whether the daemon is healthy enough to keep feeding
// the systemd watchdog. Both the sampler loop and the slow collector must
// have made progress recently; a merely-slow collector never trips it, but a
// deadlocked loop or a permanently wedged collector does.
type livenessGate struct {
	lastTick          *atomic.Int64 // unix sec of last sampler tick
	lastSlowSuccess   *atomic.Int64 // unix sec of last successful slow collection
	samplerStaleSec   int64
	collectorStaleSec int64
}

func (g livenessGate) alive(nowUnix int64) bool {
	return nowUnix-g.lastTick.Load() < g.samplerStaleSec &&
		nowUnix-g.lastSlowSuccess.Load() < g.collectorStaleSec
}

// runWatchdog pings every periodSec while the gate reports alive, until ctx
// is done. ping is sdNotify("WATCHDOG=1") in production; now/ping are
// injectable for tests.
func runWatchdog(ctx context.Context, g livenessGate, periodSec int64, now func() int64, ping func() error) {
	if periodSec < 1 {
		periodSec = 1
	}
	t := time.NewTicker(time.Duration(periodSec) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if g.alive(now()) {
				_ = ping()
			}
		}
	}
}

// watchdogPeriodSec derives the ping cadence from systemd's advertised
// WatchdogSec ($WATCHDOG_USEC), pinging at a third of it. Fallback 30s
// (a third of the unit's 90s) when unset or unparseable.
func watchdogPeriodSec() int64 {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 30
	}
	p := usec / 1_000_000 / 3
	if p < 1 {
		p = 1
	}
	return p
}
