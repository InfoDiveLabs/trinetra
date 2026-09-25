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

	// writeFrame, when non-nil, replaces the active segment's Write so tests
	// can inject a failed or partial append. nil in production.
	writeFrame func(f *os.File, b []byte) (int, error)
}

// OpenOutbox opens (creating if needed) the outbox in dir, capped at maxBytes.
func OpenOutbox(dir string, maxBytes int64) (*Outbox, error) {
	return openOutbox(dir, maxBytes, defaultSegmentMax)
}

// OpenOutboxSegmented is OpenOutbox with an explicit segment size, for tests
// and for tuning very small caps.
func OpenOutboxSegmented(dir string, maxBytes, segmentBytes int64) (*Outbox, error) {
	return openOutbox(dir, maxBytes, segmentBytes)
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
	// gaps.json is enforceCapLocked's commit point for "these records are
	// gone" (it is written before the cursor). A crash between the two
	// writes can leave the cursor on disk behind a recorded gap. The cursor
	// must never legitimately trail a recorded gap, so reconcile before
	// anything below relies on o.acked (segment cleanup, o.next).
	if o.reconcileAckedWithGaps() {
		if err := writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(o.acked, 10)), 0o600); err != nil {
			return nil, err
		}
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
	// A segment fully covered by the acked cursor is a leftover: either the
	// normal case (Ack deletes fully-acked segments but always keeps one to
	// append into) or a crash that landed after enforceCapLocked durably
	// persisted the gap/ack advance but before it deleted the evicted
	// segment file. Either way Read already skips it (seq <= after), so it
	// is safe, and simplest, to clean it up here rather than carry it
	// forward. Mirrors the eviction loop in Ack.
	for len(o.segs) > 1 && o.segs[0].last <= o.acked {
		if err := os.Remove(o.segs[0].path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		o.segs = o.segs[1:]
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

// reconcileAckedWithGaps advances o.acked to cover every recorded gap and
// reports whether it changed anything. Called once at open, before o.acked
// is used for anything else: see the comment on enforceCapLocked for why
// gaps.json can be ahead of the cursor file after a crash.
func (o *Outbox) reconcileAckedWithGaps() bool {
	changed := false
	for _, g := range o.gaps {
		if g.LastSeq > o.acked {
			o.acked = g.LastSeq
			changed = true
		}
	}
	return changed
}

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
			return 0, o.recordFailedAppendLocked(nil, seq, ts, err)
		}
		s = o.segs[len(o.segs)-1]
	}
	write := (*os.File).Write
	if o.writeFrame != nil {
		write = o.writeFrame
	}
	if _, err := write(o.cur, frame); err != nil {
		return 0, o.recordFailedAppendLocked(s, seq, ts, err)
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

// recordFailedAppendLocked handles a failed (possibly partial) write of the
// record seq/ts into segment s, the active segment (s is nil when opening a
// new segment failed, so nothing was written).
//
// A partial frame is truncated away and the segment is closed, so the next
// append starts a fresh segment and torn bytes can never sit in front of
// later frames (Read stops at the first bad frame).
//
// The record itself is then lost from the outbox, so it is handed to gap
// repair the same way a cap eviction is: every still-unacked record up to
// and including seq becomes one Gap (repaired from the local store) and the
// cursor moves to seq. Covering the whole unacked prefix rather than just
// seq keeps the outbox invariants (the cursor never trails a recorded gap;
// gaps are repaired before any later record is shipped), so the repair can
// never be applied after newer data and dropped by the master's ordering
// guard. Records before seq that are still in segments are simply re-sent
// from local history instead; the master's guard makes that idempotent.
//
// The write error is returned either way. If the gap cannot be persisted
// the outbox is left as it was before this append (seq is not issued).
func (o *Outbox) recordFailedAppendLocked(s *segment, seq uint64, ts int64, werr error) error {
	if s != nil && o.cur != nil {
		if err := o.cur.Truncate(s.size); err != nil {
			werr = fmt.Errorf("%w (and truncating the torn frame failed: %v)", werr, err)
		}
		o.cur.Close()
		o.cur = nil
		if s.count == 0 {
			_ = os.Remove(s.path)
			o.segs = o.segs[:len(o.segs)-1]
		}
	}
	g := Gap{FirstSeq: o.acked + 1, LastSeq: seq, MinTS: ts, MaxTS: ts}
	for _, sg := range o.segs {
		if sg.count == 0 || sg.last <= o.acked {
			continue
		}
		g.MinTS = min(g.MinTS, sg.minTS)
		g.MaxTS = max(g.MaxTS, sg.maxTS)
	}
	newGaps := append(append([]Gap(nil), o.gaps...), g)
	if err := o.writeGapsFile(newGaps); err != nil {
		return fmt.Errorf("%w; recording the loss as a gap also failed: %v", werr, err)
	}
	if err := writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		// gaps.json is the commit point; openOutbox reconciles the cursor.
		werr = fmt.Errorf("%w; persisting the cursor failed: %v", werr, err)
	}
	o.gaps = newGaps
	o.acked = seq
	o.next = seq + 1
	for len(o.segs) > 0 && o.segs[0].last <= o.acked {
		_ = os.Remove(o.segs[0].path)
		o.segs = o.segs[1:]
	}
	return fmt.Errorf("fleet: outbox append failed, records %d-%d handed to gap repair: %w", g.FirstSeq, g.LastSeq, werr)
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
//
// The gap/ack advance is computed and durably persisted (gaps.json, then
// cursor) BEFORE any segment file is deleted. Only once both writes succeed
// are o.segs updated and the evicted files removed. A crash (or write
// failure) before gaps.json is written leaves every evicted file on disk
// and neither o.segs, o.gaps nor o.acked touched, so the exact same drop
// set is recomputed and retried on the caller's next Append.
//
// gaps.json is written first and is the real commit point: once it names a
// range as gone, those records must never be re-served, regardless of what
// the cursor file says. A crash between the two writes therefore leaves the
// cursor trailing a recorded gap (gap.LastSeq > the on-disk acked) with the
// evicted segment file still present; openOutbox reconciles this on open
// (reconcileAckedWithGaps advances and re-persists the cursor to cover
// every recorded gap before anything else runs) so the cursor can never
// legitimately trail a recorded gap by the time segment cleanup, Read or
// Ack run. A crash after both writes succeed but before the deletes run
// leaves the same kind of harmless leftover segment file, which the
// "last <= acked" cleanup loop in openOutbox removes once the cursor is
// caught up. Either way no unacked record is ever lost without a matching
// Gap, and no already-recorded Gap's records survive to be re-delivered.
func (o *Outbox) enforceCapLocked() error {
	total := o.totalLocked()
	newGaps := append([]Gap(nil), o.gaps...)
	newAcked := o.acked
	remaining := o.segs
	var drop []*segment
	changed := false
	for total > o.max && len(remaining) > 1 {
		s := remaining[0]
		if s.last > newAcked {
			first := s.first
			if newAcked+1 > first {
				first = newAcked + 1
			}
			g := Gap{FirstSeq: first, LastSeq: s.last, MinTS: s.minTS, MaxTS: s.maxTS}
			if n := len(newGaps); n > 0 && newGaps[n-1].LastSeq+1 == g.FirstSeq {
				newGaps[n-1].LastSeq = g.LastSeq
				if g.MaxTS > newGaps[n-1].MaxTS {
					newGaps[n-1].MaxTS = g.MaxTS
				}
			} else {
				newGaps = append(newGaps, g)
			}
			newAcked = s.last
			changed = true
		}
		drop = append(drop, s)
		total -= s.size
		remaining = remaining[1:]
	}
	if len(drop) == 0 {
		return nil
	}
	if changed {
		if err := o.writeGapsFile(newGaps); err != nil {
			return err
		}
		if err := writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(newAcked, 10)), 0o600); err != nil {
			return err
		}
		o.gaps = newGaps
		o.acked = newAcked
	}
	o.segs = remaining
	for _, s := range drop {
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (o *Outbox) writeGapsFile(gaps []Gap) error {
	b, err := json.Marshal(gaps)
	if err != nil {
		return err
	}
	return writeFileAtomic(o.gapsPath(), b, 0o600)
}

func (o *Outbox) saveGapsLocked() error {
	return o.writeGapsFile(o.gaps)
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

// DivergenceError reports that the master acked a seq this outbox never
// issued: the child kept its identity but its outbox was deleted or rolled
// back (restore, rm -rf outbox/), so the master already holds a higher seq
// than anything local. Ack has already repaired the state when it returns
// this (see Ack); it is returned so the caller can log it loudly.
type DivergenceError struct {
	LocalNext   uint64 // the seq the outbox would have issued next
	MasterAcked uint64 // the seq the master acked
	Gap         *Gap   // the local unacked range handed to gap repair, if any
}

func (e *DivergenceError) Error() string {
	msg := fmt.Sprintf("fleet: outbox diverged from the master (master acked seq %d, local outbox only reached %d); local seq numbering jumps past the master's", e.MasterAcked, e.LocalNext-1)
	if e.Gap != nil {
		msg += fmt.Sprintf(", unsent records %d-%d will be re-sent from local history", e.Gap.FirstSeq, e.Gap.LastSeq)
	}
	return msg
}

// Ack records that the master durably holds everything up to seq, and
// deletes segments that are now fully acked (except the one being written).
//
// An ack beyond the last issued seq means the outbox and the master have
// diverged (see DivergenceError). Clamping it would be silent, lasting loss:
// the master drops every record with seq <= its applied seq, so every new
// record would be discarded until local numbering caught up. Instead the
// still-unacked local records become a Gap (repaired from the local store
// under the master's ordering guard), numbering resumes at seq+1, and a
// *DivergenceError is returned after the new state is durably persisted.
func (o *Outbox) Ack(seq uint64) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if seq >= o.next {
		return o.divergeLocked(seq)
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

// divergeLocked handles an ack at or beyond o.next; see Ack.
func (o *Outbox) divergeLocked(seq uint64) error {
	div := &DivergenceError{LocalNext: o.next, MasterAcked: seq}
	newGaps := append([]Gap(nil), o.gaps...)
	if o.acked+1 <= o.next-1 {
		g := Gap{FirstSeq: o.acked + 1, LastSeq: o.next - 1}
		first := true
		for _, s := range o.segs {
			if s.count == 0 || s.last < g.FirstSeq {
				continue
			}
			if first || s.minTS < g.MinTS {
				g.MinTS = s.minTS
			}
			if first || s.maxTS > g.MaxTS {
				g.MaxTS = s.maxTS
			}
			first = false
		}
		newGaps = append(newGaps, g)
		div.Gap = &g
	}
	// Same commit order as enforceCapLocked: gaps.json first, then cursor.
	if err := o.writeGapsFile(newGaps); err != nil {
		return err
	}
	if err := writeFileAtomic(o.cursorPath(), []byte(strconv.FormatUint(seq, 10)), 0o600); err != nil {
		return err
	}
	o.gaps = newGaps
	o.acked = seq
	o.next = seq + 1
	// Every local segment now lies at or below the ack. Drop them all and
	// start a fresh segment (named by the new first seq) on the next append.
	if o.cur != nil {
		o.cur.Close()
		o.cur = nil
	}
	// A file that cannot be removed is harmless: every record in it is at
	// or below the cursor, so Read skips it and openOutbox cleans it up.
	for _, s := range o.segs {
		_ = os.Remove(s.path)
	}
	o.segs = nil
	return div
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
