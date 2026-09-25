package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func appendN(t *testing.T, o *Outbox, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		if _, err := o.Append(KindSamples, int64(i), []byte(fmt.Sprintf(`{"ts":%d,"m":{"cpu":%d}}`, i, i))); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOutboxAppendReadAckReopen(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 1, 10)
	recs, err := o.Read(0, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 10 || recs[0].Seq != 1 || recs[9].Seq != 10 || recs[3].TS != 4 || recs[0].Kind != KindSamples {
		t.Fatalf("read = %+v", recs)
	}
	if err := o.Ack(5); err != nil {
		t.Fatal(err)
	}
	o.Close()

	o2, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	if o2.Acked() != 5 {
		t.Fatalf("acked after reopen = %d", o2.Acked())
	}
	recs, _ = o2.Read(o2.Acked(), 1<<20, 100)
	if len(recs) != 5 || recs[0].Seq != 6 {
		t.Fatalf("unacked after reopen = %+v", recs)
	}
	seq, _ := o2.Append(KindAlert, 99, []byte(`{}`))
	if seq != 11 {
		t.Fatalf("seq after reopen = %d, want 11", seq)
	}
	assertMode(t, filepath.Join(dir, "cursor"), 0o600)
}

func TestOutboxReadRespectsLimits(t *testing.T) {
	o, _ := OpenOutbox(t.TempDir(), 64<<20)
	defer o.Close()
	appendN(t, o, 1, 50)
	recs, _ := o.Read(0, 1<<20, 7)
	if len(recs) != 7 {
		t.Fatalf("maxRecords: got %d", len(recs))
	}
	recs, _ = o.Read(0, 1, 100) // tiny byte budget still returns one record
	if len(recs) != 1 {
		t.Fatalf("maxBytes: got %d", len(recs))
	}
}

func TestOutboxRecoversFromTornTail(t *testing.T) {
	dir := t.TempDir()
	o, _ := OpenOutbox(dir, 64<<20)
	appendN(t, o, 1, 3)
	o.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_APPEND|os.O_WRONLY, 0)
	f.Write([]byte{0, 0, 0, 40, 1, 2, 3}) // torn frame: header promises 40 bytes
	f.Close()

	o2, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	recs, _ := o2.Read(0, 1<<20, 100)
	if len(recs) != 3 {
		t.Fatalf("records after torn tail = %d", len(recs))
	}
	seq, _ := o2.Append(KindSamples, 4, []byte(`{}`))
	if seq != 4 {
		t.Fatalf("next seq = %d, want 4", seq)
	}
	recs, _ = o2.Read(0, 1<<20, 100)
	if len(recs) != 4 {
		t.Fatalf("records after append = %d", len(recs))
	}
}

func TestOutboxCorruptCRCDropsFromThere(t *testing.T) {
	dir := t.TempDir()
	o, _ := OpenOutbox(dir, 64<<20)
	appendN(t, o, 1, 3)
	o.Close()
	segs, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	b, _ := os.ReadFile(segs[0])
	b[len(b)/2] ^= 0xFF // corrupt the middle record
	os.WriteFile(segs[0], b, 0o600)

	o2, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	recs, _ := o2.Read(0, 1<<20, 100)
	if len(recs) == 0 || len(recs) >= 3 {
		t.Fatalf("expected prefix before corruption, got %d", len(recs))
	}
}

func TestOutboxCapRecordsGap(t *testing.T) {
	dir := t.TempDir()
	o, err := openOutbox(dir, 3000, 1000)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 1, 200) // ~40 bytes each, far over the 3000 byte cap
	gaps := o.Gaps()
	if len(gaps) != 1 || gaps[0].FirstSeq != 1 || gaps[0].MinTS != 1 {
		t.Fatalf("gaps = %+v", gaps)
	}
	if o.Stats().Bytes > 3000+1000 {
		t.Fatalf("outbox bytes %d exceed cap+segment", o.Stats().Bytes)
	}
	recs, _ := o.Read(o.Acked(), 1<<20, 1000)
	if len(recs) == 0 || recs[0].Seq != gaps[0].LastSeq+1 || recs[len(recs)-1].Seq != 200 {
		t.Fatalf("unacked after cap: first=%d last=%d gap=%+v", recs[0].Seq, recs[len(recs)-1].Seq, gaps[0])
	}
	o.Close()

	o2, _ := openOutbox(dir, 3000, 1000)
	defer o2.Close()
	if len(o2.Gaps()) != 1 {
		t.Fatalf("gaps not persisted: %+v", o2.Gaps())
	}
	if err := o2.ResolveGap(1); err != nil {
		t.Fatal(err)
	}
	if len(o2.Gaps()) != 0 {
		t.Fatal("gap not resolved")
	}
}

func TestOutboxAckDeletesSegments(t *testing.T) {
	dir := t.TempDir()
	o, _ := openOutbox(dir, 1<<20, 500)
	defer o.Close()
	appendN(t, o, 1, 100)
	before, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err := o.Ack(99); err != nil {
		t.Fatal(err)
	}
	after, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(before) < 3 || len(after) > 1 {
		t.Fatalf("segments before=%d after=%d", len(before), len(after))
	}
	recs, _ := o.Read(o.Acked(), 1<<20, 100)
	if len(recs) != 1 || recs[0].Seq != 100 {
		t.Fatalf("remaining = %+v", recs)
	}
}

func TestOutboxNotifyOnAppend(t *testing.T) {
	o, _ := OpenOutbox(t.TempDir(), 64<<20)
	defer o.Close()
	appendN(t, o, 1, 1)
	select {
	case <-o.Notify():
	default:
		t.Fatal("no notification after append")
	}
}

// TestOutboxCapCrashBetweenPersistAndDelete simulates a crash that lands
// after enforceCapLocked durably persists the gap/ack advance for an
// evicted segment but before it deletes that segment's file. Reopening
// must not re-read those records as unacked (Read(Acked()) must start
// right after the gap), and the leftover file must be cleaned up.
func TestOutboxCapCrashBetweenPersistAndDelete(t *testing.T) {
	dir := t.TempDir()
	// Cap far above what we write, so no real eviction happens here — we
	// construct the "persisted but not yet deleted" state by hand below.
	o, err := openOutbox(dir, 1<<20, 200)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 1, 30)
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	segs, err := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(segs)
	if len(segs) < 2 {
		t.Fatalf("want >=2 segments to set up the scenario, got %v", segs)
	}
	first, err := scanSegment(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	gap := Gap{FirstSeq: first.first, LastSeq: first.last, MinTS: first.minTS, MaxTS: first.maxTS}
	gb, err := json.Marshal([]Gap{gap})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "gaps.json"), gb, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "cursor"), []byte(strconv.FormatUint(gap.LastSeq, 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	// segs[0]'s file is deliberately left in place: this is the exact
	// on-disk state a crash between the persist and the delete leaves.

	o2, err := openOutbox(dir, 1<<20, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()

	if o2.Acked() != gap.LastSeq {
		t.Fatalf("acked = %d, want %d", o2.Acked(), gap.LastSeq)
	}
	if gaps := o2.Gaps(); len(gaps) != 1 || gaps[0] != gap {
		t.Fatalf("gaps = %+v, want [%+v]", gaps, gap)
	}
	recs, err := o2.Read(o2.Acked(), 1<<20, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[0].Seq != gap.LastSeq+1 {
		t.Fatalf("records after crash-recovered gap start at wrong seq: %+v", recs)
	}
	for _, r := range recs {
		if r.Seq <= gap.LastSeq {
			t.Fatalf("record seq %d re-read as unacked below acked cursor %d", r.Seq, o2.Acked())
		}
	}
	if _, err := os.Stat(segs[0]); !os.IsNotExist(err) {
		t.Fatalf("leftover evicted segment file not cleaned up on reopen (err=%v)", err)
	}
}

// TestOutboxReconcilesCursorWithGapsOnOpen simulates a crash between
// enforceCapLocked's two persists: gaps.json commits an eviction (so the
// records are gone for good) but the crash lands before the cursor file is
// updated to match, leaving the cursor behind the recorded gap and the
// evicted segment's file still on disk. Reopening must treat the gap as
// authoritative: advance and re-persist the cursor to the gap's LastSeq,
// never re-serve the dropped records, and clean up the stale file.
func TestOutboxReconcilesCursorWithGapsOnOpen(t *testing.T) {
	dir := t.TempDir()
	o, err := openOutbox(dir, 1<<20, 200)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 1, 30)
	if err := o.Ack(2); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}

	segs, err := filepath.Glob(filepath.Join(dir, "*.seg"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(segs)
	if len(segs) < 2 {
		t.Fatalf("want >=2 segments to set up the scenario, got %v", segs)
	}
	first, err := scanSegment(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.last <= 2 {
		t.Fatalf("test setup invalid: segs[0] (last=%d) already covered by Ack(2)", first.last)
	}
	gap := Gap{FirstSeq: first.first, LastSeq: first.last, MinTS: first.minTS, MaxTS: first.maxTS}
	gb, err := json.Marshal([]Gap{gap})
	if err != nil {
		t.Fatal(err)
	}
	// Commit the gap (as enforceCapLocked would) but deliberately leave the
	// cursor file at its earlier Ack(2) value, and segs[0]'s file in place:
	// exactly the on-disk state a crash right after this write leaves.
	if err := writeFileAtomic(filepath.Join(dir, "gaps.json"), gb, 0o600); err != nil {
		t.Fatal(err)
	}

	o2, err := openOutbox(dir, 1<<20, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()

	if o2.Acked() != gap.LastSeq {
		t.Fatalf("acked = %d, want %d (reconciled with recorded gap)", o2.Acked(), gap.LastSeq)
	}
	recs, err := o2.Read(o2.Acked(), 1<<20, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) == 0 || recs[0].Seq != gap.LastSeq+1 {
		t.Fatalf("records after reconciled gap start at wrong seq: %+v", recs)
	}
	if _, err := os.Stat(segs[0]); !os.IsNotExist(err) {
		t.Fatalf("stale evicted segment file not cleaned up on reopen (err=%v)", err)
	}
	cur, err := os.ReadFile(filepath.Join(dir, "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(cur)); got != strconv.FormatUint(gap.LastSeq, 10) {
		t.Fatalf("cursor file not reconciled: got %q, want %q", got, strconv.FormatUint(gap.LastSeq, 10))
	}
}

// TestOutboxConcurrentAppendReadAck exercises the outbox under its documented
// concurrent-use contract: one goroutine appends continuously (driving
// segment rotation and cap eviction) while another concurrently reads from
// and acks the cursor (driving segment deletion), for about a second with a
// small cap/segment size so rotation and eviction happen throughout the run.
func TestOutboxConcurrentAppendReadAck(t *testing.T) {
	dir := t.TempDir()
	o, err := openOutbox(dir, 4096, 512)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()

	const runFor = 1 * time.Second
	appenderDone := make(chan struct{})
	var appended atomic.Uint64
	var appendErr error

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(appenderDone)
		i := int64(1)
		deadline := time.Now().Add(runFor)
		for time.Now().Before(deadline) {
			seq, err := o.Append(KindSamples, i, []byte(`{"cpu":1}`))
			if err != nil {
				appendErr = err
				return
			}
			appended.Store(seq)
			i++
		}
	}()

	var delivered []uint64
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			recs, err := o.Read(o.Acked(), 1<<20, 50)
			if err != nil {
				readErr = err
				return
			}
			if len(recs) == 0 {
				select {
				case <-appenderDone:
					if o.Acked() >= appended.Load() {
						return
					}
				default:
				}
				time.Sleep(time.Millisecond)
				continue
			}
			for _, r := range recs {
				delivered = append(delivered, r.Seq)
			}
			if err := o.Ack(recs[len(recs)-1].Seq); err != nil {
				readErr = err
				return
			}
		}
	}()

	wg.Wait()
	if appendErr != nil {
		t.Fatalf("append: %v", appendErr)
	}
	if readErr != nil {
		t.Fatalf("read/ack: %v", readErr)
	}
	total := appended.Load()
	if total < 1000 {
		t.Fatalf("test didn't generate enough load: appended=%d", total)
	}

	// Seqs delivered by Read must be strictly increasing across the whole
	// run: never repeated, never delivered out of order, never re-delivered
	// once past the (monotonically advancing) acked cursor.
	for i := 1; i < len(delivered); i++ {
		if delivered[i] <= delivered[i-1] {
			t.Fatalf("delivered seqs not strictly increasing at %d: %d <= %d", i, delivered[i], delivered[i-1])
		}
	}

	// Every seq that was ever appended must be accounted for: either
	// delivered, or covered by a recorded Gap (dropped by the cap before
	// the reader could catch up).
	covered := make([]bool, total+1)
	for _, s := range delivered {
		covered[s] = true
	}
	for _, g := range o.Gaps() {
		for s := g.FirstSeq; s <= g.LastSeq; s++ {
			covered[s] = true
		}
	}
	for s := uint64(1); s <= total; s++ {
		if !covered[s] {
			t.Fatalf("seq %d neither delivered nor gapped (appended=%d, delivered=%d, gaps=%+v)", s, total, len(delivered), o.Gaps())
		}
	}
}

// TestOutboxAckBeyondNextRecordsDivergenceGap covers a child whose outbox was
// deleted or rolled back while the master kept its applied seq: the master
// acks a seq the local outbox never issued. The unacked local records must
// become a gap (repaired from local history) and seq numbering must jump past
// the master's view, instead of the ack being clamped and every new record
// silently discarded by the master as "already applied".
func TestOutboxAckBeyondNextRecordsDivergenceGap(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 10, 4) // seqs 1..4, ts 10..13; next == 5
	err = o.Ack(100)
	var div *DivergenceError
	if !errors.As(err, &div) {
		t.Fatalf("Ack(100) err = %v, want *DivergenceError", err)
	}
	if div.LocalNext != 5 || div.MasterAcked != 100 {
		t.Fatalf("divergence = %+v", div)
	}
	gaps := o.Gaps()
	if len(gaps) != 1 || gaps[0] != (Gap{FirstSeq: 1, LastSeq: 4, MinTS: 10, MaxTS: 13}) {
		t.Fatalf("gaps = %+v", gaps)
	}
	if st := o.Stats(); st.NextSeq != 101 || st.AckedSeq != 100 || st.Unacked != 0 {
		t.Fatalf("stats = %+v", st)
	}
	seq, err := o.Append(KindSamples, 20, []byte(`{}`))
	if err != nil || seq != 101 {
		t.Fatalf("append after divergence: seq %d err %v", seq, err)
	}
	recs, _ := o.Read(o.Acked(), 1<<20, 100)
	if len(recs) != 1 || recs[0].Seq != 101 {
		t.Fatalf("read after divergence = %+v", recs)
	}
	o.Close()

	o2, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	if o2.Acked() != 100 || len(o2.Gaps()) != 1 {
		t.Fatalf("after reopen acked=%d gaps=%+v", o2.Acked(), o2.Gaps())
	}
	if seq, _ := o2.Append(KindSamples, 21, []byte(`{}`)); seq != 102 {
		t.Fatalf("seq after reopen = %d, want 102", seq)
	}
}

// An empty outbox (nothing unacked) that diverges just jumps ahead; there is
// nothing to repair.
func TestOutboxAckBeyondNextOnEmptyOutboxRecordsNoGap(t *testing.T) {
	o, _ := OpenOutbox(t.TempDir(), 64<<20)
	defer o.Close()
	if err := o.Ack(50); err == nil {
		t.Fatal("want divergence error")
	}
	if len(o.Gaps()) != 0 {
		t.Fatalf("gaps = %+v", o.Gaps())
	}
	if seq, _ := o.Append(KindSamples, 1, []byte(`{}`)); seq != 51 {
		t.Fatalf("seq = %d, want 51", seq)
	}
}

// TestOutboxFailedAppendBecomesGapAndKeepsSegmentReadable injects a write
// failure that leaves half a frame on disk mid-segment. The torn bytes must
// not hide later records (they are truncated away and the next append goes to
// a fresh segment), the failed record's seq must not be silently reused, and
// its time range must become a gap so local-history backfill repairs it.
func TestOutboxFailedAppendBecomesGapAndKeepsSegmentReadable(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, o, 1, 3) // seqs 1..3, ts 1..3
	if err := o.Ack(1); err != nil {
		t.Fatal(err)
	}
	o.writeFrame = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2])
		return n, errors.New("injected: disk full")
	}
	if _, err := o.Append(KindSamples, 4, []byte(`{"ts":4}`)); err == nil {
		t.Fatal("append with failing writer succeeded")
	}
	o.writeFrame = nil
	gaps := o.Gaps()
	if len(gaps) != 1 || gaps[0].FirstSeq != 2 || gaps[0].LastSeq != 4 || gaps[0].MinTS > 2 || gaps[0].MaxTS != 4 {
		t.Fatalf("gaps = %+v, want one gap 2-4 covering ts 2..4", gaps)
	}
	appendN(t, o, 5, 2)
	recs, err := o.Read(o.Acked(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Seq != 5 || recs[0].TS != 5 || recs[1].Seq != 6 {
		t.Fatalf("records after failed append = %+v", recs)
	}
	o.Close()
	o2, err := OpenOutbox(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o2.Close()
	recs, _ = o2.Read(o2.Acked(), 1<<20, 100)
	if len(recs) != 2 || recs[0].Seq != 5 || len(o2.Gaps()) != 1 {
		t.Fatalf("after reopen: recs=%+v gaps=%+v", recs, o2.Gaps())
	}
}

// OldestUnackedTS is the ts of the first record after the ack, not the
// oldest ts in its segment: a partly acked segment must not make a caught-up
// node look like it has a large backlog (and so "lagging").
func TestOutboxOldestUnackedIsPerRecord(t *testing.T) {
	o, _ := OpenOutbox(t.TempDir(), 64<<20)
	defer o.Close()
	appendN(t, o, 100, 10) // one segment, ts 100..109
	if st := o.Stats(); st.OldestUnackedTS != 100 {
		t.Fatalf("before ack = %d, want 100", st.OldestUnackedTS)
	}
	if err := o.Ack(7); err != nil {
		t.Fatal(err)
	}
	if st := o.Stats(); st.OldestUnackedTS != 107 {
		t.Fatalf("after ack 7 = %d, want 107", st.OldestUnackedTS)
	}
	if err := o.Ack(10); err != nil {
		t.Fatal(err)
	}
	if st := o.Stats(); st.OldestUnackedTS != 0 {
		t.Fatalf("fully acked = %d, want 0", st.OldestUnackedTS)
	}
}
