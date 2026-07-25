// Package serverwatch: tsfile.go implements the default "tsfile" SampleStore
// backend described in docs/DESIGN-storage.md — a compact, append-only,
// per-series binary store.
//
// Layout under dir:
//
//	<dir>/ts/raw/<safeMetric>.tsd  — raw samples, one record per Append
//	<dir>/ts/1m/<safeMetric>.tsd   — 1-minute rollups (populated by a future
//	                                 downsampler task; Query on an absent
//	                                 file just returns no points)
//	<dir>/ts/events.tsd            — downtime events
//
// Every file starts with a fixed 16-byte header (magic, version, record
// length, resolution) followed by fixed-width records appended in
// nondecreasing timestamp order. Range queries binary-search the sorted
// records by offset instead of parsing the whole file. A torn trailing
// record (a partial write from a crash mid-append) is tolerated by simply
// excluding it from the record count — see readCount below.
//
// tsfileStore opens files per operation rather than holding descriptors
// open, so Close is a no-op; there is nothing to flush.
package serverwatch

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	tsMagic          = "SWTS"
	tsVersion        = uint16(1)
	tsRecordLen      = uint16(32) // both sample and event records are 32 bytes
	tsHeaderLen      = 16         // 4 (magic) + 2 (version) + 2 (recordLen) + 4 (resolution) + 4 (reserved)
	tsResolutionRaw  = uint32(0)
	tsResolution1m   = uint32(60)
	tsEventTypePower = uint8(1) // power_down
	tsEventTypeNet   = uint8(2) // net_down
)

// Retention windows. Real per-resolution config (storage.raw_retention /
// storage.rollup_retention) is a later task (s8); these are fixed defaults
// matching docs/DESIGN-storage.md in the meantime.
const (
	tsRawRetentionSeconds    = 48 * 3600
	tsRollupRetentionSeconds = 30 * 24 * 3600
	tsEventRetentionSeconds  = 30 * 24 * 3600
)

// tsFileStore is the on-disk binary SampleStore backend. Safe for concurrent
// use: a single RWMutex serializes writers (Append/AppendEvent/Prune)
// against each other and against readers (Query/Events), which also keeps a
// Query from ever observing a file mid-append.
type tsFileStore struct {
	mu  sync.RWMutex
	dir string // <configured dir>/ts
}

// newTSFileStore creates the tsfile directory layout under dir and returns a
// ready-to-use store.
func newTSFileStore(dir string) (*tsFileStore, error) {
	root := filepath.Join(dir, "ts")
	for _, sub := range []string{"raw", "1m"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("tsfile: create %s: %w", sub, err)
		}
	}
	return &tsFileStore{dir: root}, nil
}

// safeMetric maps a metric id to a filesystem-safe, deterministic, and
// (for the ids actually in use) collision-free filename stem. Path-unsafe
// characters are replaced with '_'.
func safeMetric(id string) string {
	if id == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == ' ' || r < 0x20:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	safe := b.String()
	if safe == "." || safe == ".." || safe == "" {
		return "_"
	}
	return safe
}

func (s *tsFileStore) resDir(res Resolution) string {
	if res == Res1m {
		return filepath.Join(s.dir, "1m")
	}
	return filepath.Join(s.dir, "raw")
}

func resolutionSeconds(res Resolution) uint32 {
	if res == Res1m {
		return tsResolution1m
	}
	return tsResolutionRaw
}

func (s *tsFileStore) metricPath(metric string, res Resolution) string {
	return filepath.Join(s.resDir(res), safeMetric(metric)+".tsd")
}

func (s *tsFileStore) eventsPath() string {
	return filepath.Join(s.dir, "events.tsd")
}

// ---- header ----

func writeTSHeader(w io.Writer, resolution uint32) error {
	var buf [tsHeaderLen]byte
	copy(buf[0:4], tsMagic)
	binary.BigEndian.PutUint16(buf[4:6], tsVersion)
	binary.BigEndian.PutUint16(buf[6:8], tsRecordLen)
	binary.BigEndian.PutUint32(buf[8:12], resolution)
	_, err := w.Write(buf[:])
	return err
}

// readTSHeader reads and validates the fixed header from f (positioned at
// its start), leaving the file offset just past the header. It returns the
// header's resolution field.
func readTSHeader(f *os.File) (uint32, error) {
	var buf [tsHeaderLen]byte
	if _, err := io.ReadFull(f, buf[:]); err != nil {
		return 0, fmt.Errorf("tsfile: read header: %w", err)
	}
	if string(buf[0:4]) != tsMagic {
		return 0, fmt.Errorf("tsfile: bad magic %q, want %q", buf[0:4], tsMagic)
	}
	version := binary.BigEndian.Uint16(buf[4:6])
	if version != tsVersion {
		return 0, fmt.Errorf("tsfile: unsupported version %d, want %d", version, tsVersion)
	}
	recLen := binary.BigEndian.Uint16(buf[6:8])
	if recLen != tsRecordLen {
		return 0, fmt.Errorf("tsfile: unexpected record length %d, want %d", recLen, tsRecordLen)
	}
	return binary.BigEndian.Uint32(buf[8:12]), nil
}

// readCount opens f's header-less body and returns the number of *whole*
// records present, silently excluding a torn trailing partial record (this
// is the corruption-tolerance mechanism: integer division floors away any
// trailing bytes short of a full record).
func readCount(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	body := fi.Size() - tsHeaderLen
	if body < 0 {
		return 0, nil
	}
	return body / int64(tsRecordLen), nil
}

func recordOffset(i int64) int64 {
	return tsHeaderLen + i*int64(tsRecordLen)
}

// appendRecord opens path (creating it with a fresh header if it doesn't
// exist yet) and appends one record. O(1): seek-to-end + one write.
func appendRecord(path string, resolution uint32, rec [32]byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		if err := writeTSHeader(f, resolution); err != nil {
			return err
		}
	}
	_, err = f.Write(rec[:])
	return err
}

// ---- sample record encode/decode ----

func encodeSampleRecord(ts int64, min, avg, max float64) [32]byte {
	var b [32]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(ts))
	binary.BigEndian.PutUint64(b[8:16], math.Float64bits(min))
	binary.BigEndian.PutUint64(b[16:24], math.Float64bits(avg))
	binary.BigEndian.PutUint64(b[24:32], math.Float64bits(max))
	return b
}

func decodeSampleRecord(b []byte) Point {
	return Point{
		TS:  int64(binary.BigEndian.Uint64(b[0:8])),
		Min: math.Float64frombits(binary.BigEndian.Uint64(b[8:16])),
		Avg: math.Float64frombits(binary.BigEndian.Uint64(b[16:24])),
		Max: math.Float64frombits(binary.BigEndian.Uint64(b[24:32])),
	}
}

func sampleRecordTS(b []byte) int64 { return int64(binary.BigEndian.Uint64(b[0:8])) }

// ---- event record encode/decode ----

func encodeEventRecord(e DownEvent) ([32]byte, error) {
	var b [32]byte
	var code uint8
	switch e.Type {
	case "power_down":
		code = tsEventTypePower
	case "net_down":
		code = tsEventTypeNet
	default:
		return b, fmt.Errorf("tsfile: unknown event type %q", e.Type)
	}
	b[0] = code
	binary.BigEndian.PutUint64(b[8:16], uint64(e.Start))
	binary.BigEndian.PutUint64(b[16:24], uint64(e.End))
	binary.BigEndian.PutUint64(b[24:32], uint64(e.DurationSec))
	return b, nil
}

func decodeEventRecord(b []byte) DownEvent {
	typ := "unknown"
	switch b[0] {
	case tsEventTypePower:
		typ = "power_down"
	case tsEventTypeNet:
		typ = "net_down"
	}
	return DownEvent{
		Type:        typ,
		Start:       int64(binary.BigEndian.Uint64(b[8:16])),
		End:         int64(binary.BigEndian.Uint64(b[16:24])),
		DurationSec: int64(binary.BigEndian.Uint64(b[24:32])),
	}
}

// ---- SampleStore implementation ----

// Append records one timestamped sample of each metric in ms to its raw
// series file (creating the file, with header, on first write).
func (s *tsFileStore) Append(ts int64, ms MetricSet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for metric, v := range ms {
		rec := encodeSampleRecord(ts, v, v, v)
		if err := appendRecord(s.metricPath(metric, ResRaw), tsResolutionRaw, rec); err != nil {
			return fmt.Errorf("tsfile: append %s: %w", metric, err)
		}
	}
	return nil
}

// Query returns the points for metric within [from, to] at the given
// resolution. An absent series file is not an error: it just means no data
// exists yet at that resolution.
func (s *tsFileStore) Query(metric string, from, to int64, res Resolution) ([]Point, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.metricPath(metric, res)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := readTSHeader(f); err != nil {
		return nil, fmt.Errorf("tsfile: %s: %w", path, err)
	}
	n, err := readCount(f)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}

	// Binary-search the first record with ts >= from (records are appended
	// in nondecreasing ts order).
	var buf [32]byte
	lo, hi := int64(0), n
	for lo < hi {
		mid := lo + (hi-lo)/2
		if _, err := f.ReadAt(buf[:8], recordOffset(mid)); err != nil {
			return nil, err
		}
		if sampleRecordTS(buf[:8]) < from {
			lo = mid + 1
		} else {
			hi = mid
		}
	}

	var out []Point
	for i := lo; i < n; i++ {
		if _, err := f.ReadAt(buf[:], recordOffset(i)); err != nil {
			return nil, err
		}
		p := decodeSampleRecord(buf[:])
		if p.TS > to {
			break
		}
		out = append(out, p)
	}
	return out, nil
}

// AppendEvent records a downtime event in events.tsd.
func (s *tsFileStore) AppendEvent(e DownEvent) error {
	rec, err := encodeEventRecord(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendRecord(s.eventsPath(), tsResolutionRaw, rec)
}

// Events returns downtime events overlapping [from, to] (End>=from &&
// Start<=to), mirroring memStore's semantics.
func (s *tsFileStore) Events(from, to int64) ([]DownEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.eventsPath()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if _, err := readTSHeader(f); err != nil {
		return nil, fmt.Errorf("tsfile: %s: %w", path, err)
	}
	n, err := readCount(f)
	if err != nil {
		return nil, err
	}

	var out []DownEvent
	var buf [32]byte
	for i := int64(0); i < n; i++ {
		if _, err := f.ReadAt(buf[:], recordOffset(i)); err != nil {
			return nil, err
		}
		e := decodeEventRecord(buf[:])
		if e.End >= from && e.Start <= to {
			out = append(out, e)
		}
	}
	return out, nil
}

// pruneFileGeneric rewrites path keeping only records for which keep
// returns true. It is crash-safe: it builds the new contents in a temp file
// and atomically renames it over path, so a crash mid-prune leaves either
// the untouched original or the fully-written replacement, never a partial
// file. A missing path is not an error (nothing to prune).
func pruneFileGeneric(path string, keep func(rec []byte) bool) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	resolution, err := readTSHeader(f)
	if err != nil {
		return fmt.Errorf("tsfile: prune %s: %w", path, err)
	}
	n, err := readCount(f)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := writeTSHeader(out, resolution); err != nil {
		out.Close()
		return err
	}
	var buf [32]byte
	for i := int64(0); i < n; i++ {
		if _, err := f.ReadAt(buf[:], recordOffset(i)); err != nil {
			out.Close()
			return err
		}
		if keep(buf[:]) {
			if _, err := out.Write(buf[:]); err != nil {
				out.Close()
				return err
			}
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *tsFileStore) pruneDir(dir string, cut int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keep := func(rec []byte) bool { return sampleRecordTS(rec) >= cut }
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".tsd") {
			continue
		}
		if err := pruneFileGeneric(filepath.Join(dir, ent.Name()), keep); err != nil {
			return err
		}
	}
	return nil
}

// Prune drops samples and events older than fixed retention windows
// relative to now (see tsRawRetentionSeconds etc; per-resolution
// configuration is a later task).
func (s *tsFileStore) Prune(now int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.pruneDir(filepath.Join(s.dir, "raw"), now-tsRawRetentionSeconds); err != nil {
		return err
	}
	if err := s.pruneDir(filepath.Join(s.dir, "1m"), now-tsRollupRetentionSeconds); err != nil {
		return err
	}
	eventCut := now - tsEventRetentionSeconds
	keepEvent := func(rec []byte) bool {
		end := int64(binary.BigEndian.Uint64(rec[16:24]))
		return end >= eventCut
	}
	return pruneFileGeneric(s.eventsPath(), keepEvent)
}

// Close releases any resources held by the store. tsFileStore opens files
// per operation rather than holding descriptors open, so there is nothing
// to flush or close here.
func (s *tsFileStore) Close() error { return nil }
