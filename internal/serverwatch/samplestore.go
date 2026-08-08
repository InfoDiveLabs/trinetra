// Package serverwatch: samplestore.go defines the SampleStore abstraction
// described in docs/handbook/09-storage-and-data-model.md -- the swappable seam between the
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
	"time"
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
// depends on instead of a concrete file format. See docs/handbook/09-storage-and-data-model.md.
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
	// PurgeEvents rewrites the downtime-event log keeping only events for which
	// keep returns true, reporting how many were removed. The `downtime purge`
	// CLI uses it to clear bogus events, e.g. a crash loop's fabricated short
	// power_downs (#116).
	PurgeEvents(keep func(DownEvent) bool) (int, error)
	// Prune applies the backend's retention policy relative to nowUnix.
	Prune(nowUnix int64) error
	// Downsample rolls completed raw buckets into the 1m resolution, relative
	// to nowUnix (a bucket is "completed" once its end has passed). It is
	// idempotent: calling it repeatedly with the same or later nowUnix never
	// re-rolls or duplicates a bucket already written. Backends that don't
	// distinguish resolutions (memStore) may no-op.
	Downsample(nowUnix int64) error
	// Close releases any resources held by the backend.
	Close() error
	// Stats reports the storage engine's cardinality/disk cost: seriesCount
	// is the number of distinct time series the backend is tracking
	// (tsFileStore: the number of .tsd files on disk across raw/1m/events;
	// memStore: the number of in-memory metric series) and diskBytes is the
	// total bytes those series occupy on disk (always 0 for memStore, which
	// is non-persistent). `serverwatch doctor` surfaces this as a
	// cardinality/disk guardrail (docs/handbook/12-roadmap-and-status.md Epic #69 x7).
	Stats() (seriesCount int, diskBytes int64, err error)
}

// StoreOptions configures a SampleStore's per-resolution retention policy
// (docs/handbook/09-storage-and-data-model.md "Resolutions, downsampling & retention"). Zero
// values are replaced with sensible defaults (48h/720h/720h) by each
// backend's constructor, so callers may pass a bare StoreOptions{} to get
// the documented defaults.
type StoreOptions struct {
	RawRetention    time.Duration
	RollupRetention time.Duration
	EventRetention  time.Duration
}

// defaultRawRetention/defaultRollupRetention/defaultEventRetention match the
// defaults documented in docs/handbook/09-storage-and-data-model.md and config.Default().
const (
	defaultRawRetention    = 48 * time.Hour
	defaultRollupRetention = 720 * time.Hour // 30d
	defaultEventRetention  = 720 * time.Hour // 30d
)

// withDefaults returns opts with any zero/negative field replaced by the
// package defaults.
func (o StoreOptions) withDefaults() StoreOptions {
	if o.RawRetention <= 0 {
		o.RawRetention = defaultRawRetention
	}
	if o.RollupRetention <= 0 {
		o.RollupRetention = defaultRollupRetention
	}
	if o.EventRetention <= 0 {
		o.EventRetention = defaultEventRetention
	}
	return o
}

// PickResolution returns the coarsest resolution that still satisfies a
// query over [from, to]: ResRaw when the range's start falls within the
// high-resolution window (from >= nowUnix - rawRetention), else Res1m. This
// mirrors docs/handbook/09-storage-and-data-model.md: "Query picks the coarsest resolution that
// satisfies the requested range (recent = raw, long = 1m)". Pure function;
// callers combine it with Query.
func PickResolution(from, to int64, nowUnix int64, rawRetention time.Duration) Resolution {
	cutoff := nowUnix - int64(rawRetention.Seconds())
	if from >= cutoff {
		return ResRaw
	}
	return Res1m
}

// OpenStore constructs a SampleStore for the named backend rooted at dir,
// applying opts (retention policy) to it.
//
// This is the extension point called out in docs/handbook/09-storage-and-data-model.md: adding a
// new backend (the "tsfile" default in s7, or a future "sqlite"/"remote")
// means adding a case here, not touching any caller. "memory" is a reference
// backend usable today: fully functional but non-persistent, intended for
// tests and the storage.backend=memory config option.
func OpenStore(backend, dir string, opts StoreOptions) (SampleStore, error) {
	switch backend {
	case "memory":
		return newMemStore(opts), nil
	case "tsfile":
		return newTSFileStore(dir, opts)
	default:
		return nil, fmt.Errorf("storage backend %q not implemented yet", backend)
	}
}

// memStore is an in-memory reference implementation of SampleStore. It does
// not downsample: Query returns whatever was Append-ed for the metric,
// regardless of the requested Resolution, and Downsample is a no-op -- a
// single series backs every resolution, so nothing needs rolling up. Because
// Res1m queries are served from the same raw series, Prune bounds it by
// opts.RollupRetention (the longer of the two windows) rather than
// RawRetention, so 1m-resolution history isn't lost early. Safe for
// concurrent use.
type memStore struct {
	mu     sync.Mutex
	series map[string][]Point
	events []DownEvent
	opts   StoreOptions
}

func newMemStore(opts StoreOptions) *memStore {
	return &memStore{series: make(map[string][]Point), opts: opts.withDefaults()}
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

func (m *memStore) PurgeEvents(keep func(DownEvent) bool) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.events[:0]
	removed := 0
	for _, e := range m.events {
		if keep(e) {
			kept = append(kept, e)
		} else {
			removed++
		}
	}
	m.events = kept
	return removed, nil
}

// Events returns downtime events overlapping [from, to] -- i.e. End>=from &&
// Start<=to -- mirroring the existing Store.DownSince semantics (End>=since).
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

// Prune drops points older than nowUnix - RollupRetention (see the memStore
// doc comment for why RollupRetention, not RawRetention, bounds the single
// series) and events older than nowUnix - EventRetention.
func (m *memStore) Prune(nowUnix int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cut := nowUnix - int64(m.opts.RollupRetention.Seconds())
	for metric, pts := range m.series {
		kept := pts[:0:0]
		for _, p := range pts {
			if p.TS >= cut {
				kept = append(kept, p)
			}
		}
		m.series[metric] = kept
	}
	eventCut := nowUnix - int64(m.opts.EventRetention.Seconds())
	kept := m.events[:0:0]
	for _, e := range m.events {
		if e.End >= eventCut {
			kept = append(kept, e)
		}
	}
	m.events = kept
	return nil
}

// Downsample is a no-op for memStore: it keeps only raw data and serves any
// resolution from that same series (see the type doc comment), so there is
// nothing to roll up.
func (m *memStore) Downsample(nowUnix int64) error { return nil }

func (m *memStore) Close() error { return nil }

// Stats reports the number of in-memory metric series and always 0
// diskBytes (memStore is non-persistent; see the SampleStore.Stats doc
// comment).
func (m *memStore) Stats() (int, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.series), 0, nil
}
