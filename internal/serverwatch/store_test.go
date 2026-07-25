package serverwatch

import (
	"testing"
	"time"
)

type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

func TestSampleAppendAndPrune(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_000_000_000, 0)
	s := NewStore(dir, fixedClock{now})
	if err := s.AppendSample(Sample{TS: now.Unix(), CPU: 10}); err != nil {
		t.Fatal(err)
	}
	// Simulate an old file by writing a sample dated 40 days ago.
	old := NewStore(dir, fixedClock{now.AddDate(0, 0, -40)})
	if err := old.AppendSample(Sample{TS: now.AddDate(0, 0, -40).Unix()}); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneOlderThan(30); err != nil {
		t.Fatal(err)
	}
	files, _ := osFS{}.Glob(dir + "/samples/*.jsonl")
	if len(files) != 1 {
		t.Fatalf("after prune want 1 sample file, got %d", len(files))
	}
}

func TestDownAppendAndQuery(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(2_000_000_000, 0)
	s := NewStore(dir, fixedClock{now})
	_ = s.AppendDown(DownEvent{Type: "power_down", Start: now.Unix() - 100, End: now.Unix(), DurationSec: 100})
	evs, err := s.DownSince(now.Unix() - 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "power_down" {
		t.Fatalf("events = %+v", evs)
	}
}
