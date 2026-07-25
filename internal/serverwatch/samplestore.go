// Package serverwatch: samplestore.go defines the SampleStore abstraction
// described in docs/DESIGN-storage.md — the swappable seam between the
// daemon/handlers/digests and the concrete time-series storage engine.
//
// The existing JSONL Store (store.go) remains the live backend used by
// callers for now; this file adds the new interface plus an in-memory
// reference backend used for tests and the "memory" storage.backend option.
// The default "tsfile" backend lives in tsfile.go (s7). A later task (s10)
// migrates callers onto SampleStore.
package serverwatch

import (
	"fmt"
	"sort"
	"sync"
)

// MetricSet maps a metric id (e.g. "cpu", "mem", "disk:/") to its current
// value at a single point in time.
type MetricSet map[string]float64

// Point is one time-series sample. Raw points set Min=Avg=Max=value; rolled
// up points (a future resolution's job) carry a real min/avg/max over the
// rollup window.
type Point struct {
	TS  int64
	Min float64
	Avg float64
	Max float64
}

// Resolution names a query granularity. Backends may satisfy any resolution
// with the same underlying data (as memStore does) or maintain distinct
// per-resolution series (as the future tsfile backend will).
type Resolution string

const (
	ResRaw Resolution = "raw"
	Res1m  Resolution = "1m"
)

// SampleStore is the storage seam every caller (daemon, handlers, digests)
// depends on instead of a concrete file format. See docs/DESIGN-storage.md.
type SampleStore interface {
	// Append records one timestamped sample of one or more metrics.
	Append(ts int64, m MetricSet) error
	// Query returns the points for metric within [from, to] at the given
	// resolution, ordered by TS ascending.
	Query(metric string, from, to int64, res Resolution) ([]Point, error)
	// AppendEvent records a downtime event.
	AppendEvent(e DownEvent) error
	// Events returns downtime events overlapping [from, to].
	Events(from, to int64) ([]DownEvent, error)
	// Prune applies the backend's retention policy relative to nowUnix.
	Prune(nowUnix int64) error
	// Close releases any resources held by the backend.
	Close() error
}

// OpenStore constructs a SampleStore for the named backend rooted at dir.
//
// This is the extension point called out in docs/DESIGN-storage.md: adding a
// new backend (the "tsfile" default in s7, or a future "sqlite"/"remote")
// means adding a case here, not touching any caller. "memory" is a reference
// backend usable today: fully functional but non-persistent, intended for
// tests and the storage.backend=memory config option.
func OpenStore(backend, dir string) (SampleStore, error) {
	switch backend {
	case "memory":
		return newMemStore(), nil
	case "tsfile":
		return newTSFileStore(dir)
	default:
		return nil, fmt.Errorf("storage backend %q not implemented yet", backend)
	}
}

// memRetentionSeconds is a simple fixed retention window for the in-memory
// backend. Real per-resolution retention policy (storage.raw_retention /
// storage.rollup_retention) is a later task (s8) that applies to the tsfile
// backend; memStore just needs "some" bound so long-running tests/processes
// don't grow without limit.
const memRetentionSeconds = 30 * 24 * 60 * 60 // 30 days

// memStore is an in-memory reference implementation of SampleStore. It does
// not downsample: Query returns whatever was Append-ed for the metric,
// regardless of the requested Resolution (downsampling raw->1m is the
// tsfile/s8 backend's job). Safe for concurrent use.
type memStore struct {
	mu     sync.Mutex
	series map[string][]Point
	events []DownEvent
}

func newMemStore() *memStore {
	return &memStore{series: make(map[string][]Point)}
}

func (m *memStore) Append(ts int64, ms MetricSet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for metric, v := range ms {
		m.series[metric] = append(m.series[metric], Point{TS: ts, Min: v, Avg: v, Max: v})
	}
	return nil
}

// Query returns points for metric within [from, to], inclusive, ordered by
// TS ascending. res is accepted but ignored (see memStore doc comment).
func (m *memStore) Query(metric string, from, to int64, res Resolution) ([]Point, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pts := m.series[metric]
	out := make([]Point, 0, len(pts))
	for _, p := range pts {
		if p.TS >= from && p.TS <= to {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	return out, nil
}

func (m *memStore) AppendEvent(e DownEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

// Events returns downtime events overlapping [from, to] — i.e. End>=from &&
// Start<=to — mirroring the existing Store.DownSince semantics (End>=since).
func (m *memStore) Events(from, to int64) ([]DownEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]DownEvent, 0, len(m.events))
	for _, e := range m.events {
		if e.End >= from && e.Start <= to {
			out = append(out, e)
		}
	}
	return out, nil
}

// Prune drops points and events older than nowUnix - memRetentionSeconds.
func (m *memStore) Prune(nowUnix int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := nowUnix - memRetentionSeconds
	for metric, pts := range m.series {
		kept := pts[:0:0]
		for _, p := range pts {
			if p.TS >= cut {
				kept = append(kept, p)
			}
		}
		m.series[metric] = kept
	}
	kept := m.events[:0:0]
	for _, e := range m.events {
		if e.End >= cut {
			kept = append(kept, e)
		}
	}
	m.events = kept
	return nil
}

func (m *memStore) Close() error { return nil }
