package pluginhost

import (
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

const maxLogRunes = 1024

type bucket struct {
	tokens float64
	last   time.Time
}

type statusEntry struct {
	report plugin.StatusReport
	at     int64
}

// LogSink rate-limits, sanitizes and records plugin log lines and holds each plugin's reported status.
type LogSink struct {
	logf    func(format string, args ...any)
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
	dropped map[string]int64
	recent  map[string][]plugin.LogEntry
	status  map[string]statusEntry
}

// NewLogSink returns a LogSink writing accepted lines through logf.
func NewLogSink(logf func(format string, args ...any), now func() time.Time) *LogSink {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if now == nil {
		now = time.Now
	}
	return &LogSink{logf: logf, now: now, buckets: map[string]*bucket{}, dropped: map[string]int64{},
		recent: map[string][]plugin.LogEntry{}, status: map[string]statusEntry{}}
}

func cleanLog(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxLogRunes {
		s = string(r[:maxLogRunes])
	}
	return s
}

// Log records e for id, dropping it (and counting) when id exceeds its rate.
func (l *LogSink) Log(id string, e plugin.LogEntry) error {
	switch e.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level %q must be debug, info, warn or error", e.Level)
	}
	now := l.now()
	l.mu.Lock()
	b := l.buckets[id]
	if b == nil {
		b = &bucket{tokens: plugin.LogBurst, last: now}
		l.buckets[id] = b
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * plugin.LogRatePerSec
		if b.tokens > plugin.LogBurst {
			b.tokens = plugin.LogBurst
		}
		b.last = now
	}
	if b.tokens < 1 {
		l.dropped[id]++
		l.mu.Unlock()
		return nil
	}
	b.tokens--
	e.Message = cleanLog(e.Message)
	if e.Time == 0 {
		e.Time = now.Unix()
	}
	ring := append(l.recent[id], e)
	if len(ring) > plugin.RecentLogLines {
		ring = ring[len(ring)-plugin.RecentLogLines:]
	}
	l.recent[id] = ring
	l.mu.Unlock()
	l.logf("plugin %s: [%s] %s", id, e.Level, e.Message)
	return nil
}

// Dropped returns how many of id's log lines were rate-limited.
func (l *LogSink) Dropped(id string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dropped[id]
}

// Recent returns id's last accepted log lines, oldest first.
func (l *LogSink) Recent(id string) []plugin.LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]plugin.LogEntry(nil), l.recent[id]...)
}

// SetStatus records id's self-reported status at unix time now.
func (l *LogSink) SetStatus(id string, r plugin.StatusReport, now int64) error {
	switch r.State {
	case "ok", "degraded", "error":
	default:
		return fmt.Errorf("status state %q must be ok, degraded or error", r.State)
	}
	r.Message = cleanLog(r.Message)
	l.mu.Lock()
	l.status[id] = statusEntry{report: r, at: now}
	l.mu.Unlock()
	return nil
}

// Status returns id's last reported status and when it was reported.
func (l *LogSink) Status(id string) (plugin.StatusReport, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.status[id]
	return s.report, s.at
}

// Forget drops all state held for id.
func (l *LogSink) Forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, id)
	delete(l.dropped, id)
	delete(l.recent, id)
	delete(l.status, id)
}
