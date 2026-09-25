package fleet

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const defaultSegmentMax = 4 << 20

var kindCode = map[string]byte{KindSamples: 1, KindDownEvent: 2, KindAlert: 3}
var kindName = map[byte]string{1: KindSamples, 2: KindDownEvent, 3: KindAlert}

// Gap is a run of records dropped by the outbox cap before the master acked
// them. The shipper repairs it from the child's local store.
type Gap struct {
	FirstSeq uint64 `json:"first_seq"`
	LastSeq  uint64 `json:"last_seq"`
	MinTS    int64  `json:"min_ts"`
	MaxTS    int64  `json:"max_ts"`
}

type segment struct {
	path         string
	first, last  uint64
	minTS, maxTS int64
	size         int64
	count        int
}

// Outbox is a child's durable, bounded, append-only spool of telemetry
// records awaiting the master's ack. Safe for concurrent use: the store
// writer goroutine appends while the shipper reads and acks.
type Outbox struct {
	dir    string
	max    int64
	segMax int64

	mu     sync.Mutex
	segs   []*segment
	cur    *os.File
	next   uint64
	acked  uint64
	gaps   []Gap
	notify chan struct{}
}

// OpenOutbox opens (creating if needed) the outbox in dir, capped at maxBytes.
func OpenOutbox(dir string, maxBytes int64) (*Outbox, error) {
	return openOutbox(dir, maxBytes, defaultSegmentMax)
}

func openOutbox(dir string, maxBytes, segMax int64) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	o := &Outbox{dir: dir, max: maxBytes, segMax: segMax, notify: make(chan struct{}, 1)}
	if b, err := os.ReadFile(o.cursorPath()); err == nil {
		o.acked, _ = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	}
	if b, err := os.ReadFile(o.gapsPath()); err == nil {
		_ = json.Unmarshal(b, &o.gaps)
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	for _, name := range names {
		s, err := scanSegment(name)
		if err != nil {
			return nil, err
		}
		if s.count == 0 {
			os.Remove(name)
			continue
		}
		o.segs = append(o.segs, s)
	}
	o.next = o.acked + 1
	if n := len(o.segs); n > 0 && o.segs[n-1].last+1 > o.next {
		o.next = o.segs[n-1].last + 1
	}
	if n := len(o.segs); n > 0 && o.segs[n-1].size < o.segMax {
		f, err := os.OpenFile(o.segs[n-1].path, os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		o.cur = f
	}
	return o, nil
}

func (o *Outbox) cursorPath() string { return filepath.Join(o.dir, "cursor") }
func (o *Outbox) gapsPath() string   { return filepath.Join(o.dir, "gaps.json") }

// scanSegment reads every valid frame in path, truncating the file at the
// first torn or corrupt frame (a crash mid-write), and returns its metadata.
func scanSegment(path string) (*segment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &segment{path: path}
	off := 0
	for {
		rec, n, ok := decodeFrame(b[off:])
		if !ok {
			break
		}
		if s.count == 0 {
			s.first, s.minTS, s.maxTS = rec.Seq, rec.TS, rec.TS
		}
		s.last = rec.Seq
		if rec.TS < s.minTS {
			s.minTS = rec.TS
		}
		if rec.TS > s.maxTS {
			s.maxTS = rec.TS
		}
		s.count++
		off += n
	}
	if off < len(b) {
		if err := os.Truncate(path, int64(off)); err != nil {
			return nil, err
		}
	}
	s.size = int64(off)
	return s, nil
}

func encodeFrame(seq uint64, kind byte, ts int64, data []byte) []byte {
	body := make([]byte, 0, len(data)+24)
	body = binary.AppendUvarint(body, seq)
	body = append(body, kind)
	body = binary.AppendVarint(body, ts)
	body = append(body, data...)
	frame := make([]byte, 4, len(body)+8)
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	frame = append(frame, body...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(body))
}

// decodeFrame parses one frame at the start of b, returning the record, the
// frame length, and false if b holds no complete valid frame.
func decodeFrame(b []byte) (Record, int, bool) {
	if len(b) < 8 {
		return Record{}, 0, false
	}
	n := int(binary.BigEndian.Uint32(b))
	if n < 3 || len(b) < 8+n {
		return Record{}, 0, false
	}
	body := b[4 : 4+n]
	if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(b[4+n:]) {
		return Record{}, 0, false
	}
	seq, k := binary.Uvarint(body)
	if k <= 0 || k >= len(body) {
		return Record{}, 0, false
	}
	kind, ok := kindName[body[k]]
	if !ok {
		return Record{}, 0, false
	}
	ts, k2 := binary.Varint(body[k+1:])
	if k2 <= 0 {
		return Record{}, 0, false
	}
	data := append([]byte(nil), body[k+1+k2:]...)
	return Record{Seq: seq, Kind: kind, TS: ts, Data: data}, 8 + n, true
}

// Append adds one record and returns its seq.
func (o *Outbox) Append(kind string, ts int64, data []byte) (uint64, error) {
	code, ok := kindCode[kind]
	if !ok {
		return 0, fmt.Errorf("fleet: unknown record kind %q", kind)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	seq := o.next
	frame := encodeFrame(seq, code, ts, data)
	var s *segment
	if n := len(o.segs); n > 0 && o.cur != nil && o.segs[n-1].size+int64(len(frame)) <= o.segMax {
		s = o.segs[n-1]
	} else {
		if err := o.rotateLocked(seq); err != nil {
			return 0, err
		}
		s = o.segs[len(o.segs)-1]
	}
	if _, err := o.cur.Write(frame); err != nil {
		return 0, err
	}
	if s.count == 0 {
		s.first, s.minTS, s.maxTS = seq, ts, ts
	}
	s.last = seq
	if ts < s.minTS {
		s.minTS = ts
	}
	if ts > s.maxTS {
		s.maxTS = ts
	}
	s.size += int64(len(frame))
	s.count++
	o.next++
	if err := o.enforceCapLocked(); err != nil {
		return seq, err
	}
	select {
	case o.notify <- struct{}{}:
	default:
	}
	return seq, nil
}

func (o *Outbox) rotateLocked(firstSeq uint64) error {
	if o.cur != nil {
		if err := o.cur.Sync(); err != nil {
			return err
		}
		o.cur.Close()
		o.cur = nil
	}
	path := filepath.Join(o.dir, fmt.Sprintf("%020d.seg", firstSeq))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	o.cur = f
	o.segs = append(o.segs, &segment{path: path})
	return nil
}

func (o *Outbox) totalLocked() int64 {
	var t int64
	for _, s := range o.segs {
		t += s.size
	}
	return t
}

// enforceCapLocked drops the oldest segments (never the one being written)
// while over the cap, recording any unacked records they held as a gap.
func (o *Outbox) enforceCapLocked() error {
	changed := false
	for o.totalLocked() > o.max && len(o.segs) > 1 {
		s := o.segs[0]
		if s.last > o.acked {
			first := s.first
			if o.acked+1 > first {
				first = o.acked + 1
			}
			g := Gap{FirstSeq: first, LastSeq: s.last, MinTS: s.minTS, MaxTS: s.maxTS}
			if n := len(o.gaps); n > 0 && o.gaps[n-1].LastSeq+1 == g.FirstSeq {
				o.gaps[n-1].LastSeq = g.LastSeq
				if g.MaxTS > o.gaps[n-1].MaxTS {
					o.gaps[n-1].MaxTS = g.MaxTS
				}
			} else {
				o.gaps = append(o.gaps, g)
			}
			o.acked = s.last
			changed = true
		}
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		o.segs = o.segs[1:]
	}
	if !changed {
		return nil
	}
	if err := o.saveGapsLocked(); err != nil {
		return err
	}
	return writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(o.acked, 10)), 0o600)
}

func (o *Outbox) saveGapsLocked() error {
	b, err := json.Marshal(o.gaps)
	if err != nil {
		return err
	}
	return writeFileAtomic(o.gapsPath(), b, 0o600)
}

// Read returns up to maxRecords records with seq > after, stopping once
// maxBytes of payload is reached (always at least one record if any exist).
func (o *Outbox) Read(after uint64, maxBytes, maxRecords int) ([]Record, error) {
	o.mu.Lock()
	segs := make([]segment, len(o.segs))
	for i, s := range o.segs {
		segs[i] = *s
	}
	o.mu.Unlock()

	var out []Record
	total := 0
	for _, s := range segs {
		if s.last <= after {
			continue
		}
		f, err := os.Open(s.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // dropped by the cap concurrently
			}
			return nil, err
		}
		b := make([]byte, s.size)
		_, err = io.ReadFull(f, b)
		f.Close()
		if err != nil {
			return nil, err
		}
		off := 0
		for off < len(b) {
			rec, n, ok := decodeFrame(b[off:])
			if !ok {
				break
			}
			off += n
			if rec.Seq <= after {
				continue
			}
			if len(out) > 0 && (len(out) >= maxRecords || total+len(rec.Data) > maxBytes) {
				return out, nil
			}
			out = append(out, rec)
			total += len(rec.Data)
		}
	}
	return out, nil
}

// Ack records that the master durably holds everything up to seq, and
// deletes segments that are now fully acked (except the one being written).
func (o *Outbox) Ack(seq uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if seq >= o.next {
		seq = o.next - 1
	}
	if seq <= o.acked {
		return nil
	}
	o.acked = seq
	if err := writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		return err
	}
	for len(o.segs) > 1 && o.segs[0].last <= o.acked {
		if err := os.Remove(o.segs[0].path); err != nil && !os.IsNotExist(err) {
			return err
		}
		o.segs = o.segs[1:]
	}
	return nil
}

// Acked returns the highest acked seq.
func (o *Outbox) Acked() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.acked
}

// Gaps returns a copy of the unrepaired gaps, oldest first.
func (o *Outbox) Gaps() []Gap {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Gap(nil), o.gaps...)
}

// ResolveGap forgets the gap starting at first once it has been backfilled.
func (o *Outbox) ResolveGap(first uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, g := range o.gaps {
		if g.FirstSeq == first {
			o.gaps = append(o.gaps[:i], o.gaps[i+1:]...)
			return o.saveGapsLocked()
		}
	}
	return nil
}

// Stats summarizes the spool.
func (o *Outbox) Stats() OutboxStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := OutboxStats{Bytes: o.totalLocked(), NextSeq: o.next, AckedSeq: o.acked, Gaps: len(o.gaps)}
	if o.next-1 > o.acked {
		st.Unacked = o.next - 1 - o.acked
	}
	for _, s := range o.segs {
		if s.last > o.acked {
			st.OldestUnackedTS = s.minTS
			break
		}
	}
	return st
}

// Notify fires (coalesced) after each Append.
func (o *Outbox) Notify() <-chan struct{} { return o.notify }

// Close syncs and closes the active segment.
func (o *Outbox) Close() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cur == nil {
		return nil
	}
	err := o.cur.Sync()
	o.cur.Close()
	o.cur = nil
	return err
}
