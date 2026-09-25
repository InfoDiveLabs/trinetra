package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
