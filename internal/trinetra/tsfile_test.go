package trinetra

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTSFile(t *testing.T, dir string, opts StoreOptions) SampleStore {
	t.Helper()
	s, err := OpenStore("tsfile", dir, opts)
	if err != nil {
		t.Fatalf("OpenStore(tsfile): %v", err)
	}
	return s
}

func TestTSFileAppendQuery(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	if err := s.Append(100, MetricSet{"cpu": 10, "mem": 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(200, MetricSet{"cpu": 20, "mem": 60}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(300, MetricSet{"cpu": 30}); err != nil {
		t.Fatal(err)
	}

	pts, err := s.Query("cpu", 100, 200, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("cpu[100,200] = %+v, want 2 points", pts)
	}
	if pts[0].TS != 100 || pts[0].Min != 10 || pts[0].Avg != 10 || pts[0].Max != 10 {
		t.Fatalf("point 0 = %+v", pts[0])
	}
	if pts[1].TS != 200 || pts[1].Avg != 20 {
		t.Fatalf("point 1 = %+v", pts[1])
	}

	// out of range excluded
	pts, err = s.Query("cpu", 400, 500, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Fatalf("cpu[400,500] = %+v, want 0 points", pts)
	}

	// wrong metric only returns its own series
	pts, err = s.Query("mem", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("mem points = %+v, want 2", pts)
	}

	// metric never appended -> empty, no error
	pts, err = s.Query("swap", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Fatalf("swap points = %+v, want 0", pts)
	}
}

func TestTSFileBinarySearchSubRange(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	const n = 1000
	for i := 0; i < n; i++ {
		ts := int64(i)
		if err := s.Append(ts, MetricSet{"load1": float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	pts, err := s.Query("load1", 400, 410, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 11 {
		t.Fatalf("load1[400,410] = %d points, want 11", len(pts))
	}
	for i, p := range pts {
		wantTS := int64(400 + i)
		if p.TS != wantTS || p.Avg != float64(wantTS) {
			t.Fatalf("point %d = %+v, want ts=%d", i, p, wantTS)
		}
	}

	// window at the very start
	pts, err = s.Query("load1", 0, 2, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("load1[0,2] = %d points, want 3", len(pts))
	}

	// window at the very end
	pts, err = s.Query("load1", n-3, n+100, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("load1[%d,%d] = %d points, want 3", n-3, n+100, len(pts))
	}
}

func TestTSFilePersistence(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})

	if err := s.Append(1, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(2, MetricSet{"cpu": 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(DownEvent{Type: "power_down", Start: 1, End: 2, DurationSec: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openTSFile(t, dir, StoreOptions{})
	defer s2.Close()

	pts, err := s2.Query("cpu", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("reopened cpu points = %+v, want 2", pts)
	}

	evs, err := s2.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "power_down" {
		t.Fatalf("reopened events = %+v, want 1 power_down", evs)
	}
}

func TestTSFileBadMagicHeaderError(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	if err := s.Append(1, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "ts", "raw", "cpu.tsd")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b[0] = 'X' // corrupt magic
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Query("cpu", 0, 1000, ResRaw); err == nil {
		t.Fatal("Query on bad-magic file: want error, got nil")
	}
}

func TestTSFileCorruptionTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	for i := int64(1); i <= 5; i++ {
		if err := s.Append(i*10, MetricSet{"cpu": float64(i)}); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(dir, "ts", "raw", "cpu.tsd")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Truncate a few bytes off the end, tearing the last record.
	if err := os.Truncate(path, fi.Size()-5); err != nil {
		t.Fatal(err)
	}

	pts, err := s.Query("cpu", 0, 1000, ResRaw)
	if err != nil {
		t.Fatalf("Query on torn file: want no error, got %v", err)
	}
	if len(pts) != 4 {
		t.Fatalf("Query on torn file = %d points, want 4 intact records", len(pts))
	}
	for i, p := range pts {
		want := int64((i + 1) * 10)
		if p.TS != want {
			t.Fatalf("point %d ts = %d, want %d", i, p.TS, want)
		}
	}
}

func TestTSFileEventsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	e1 := DownEvent{Type: "power_down", Start: 100, End: 200, DurationSec: 100}
	e2 := DownEvent{Type: "net_down", Start: 500, End: 600, DurationSec: 100}
	if err := s.AppendEvent(e1); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(e2); err != nil {
		t.Fatal(err)
	}

	evs, err := s.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want 2", evs)
	}
	if evs[0].Type != "power_down" || evs[1].Type != "net_down" {
		t.Fatalf("event type codes did not round-trip: %+v", evs)
	}

	evs, err = s.Events(150, 250)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != "power_down" {
		t.Fatalf("events[150,250] = %+v, want just power_down", evs)
	}

	evs, err = s.Events(700, 800)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("events[700,800] = %+v, want none", evs)
	}
}

// TestTSFilePurgeEvents pins the #116 purge tool: it removes only the events the predicate
// rejects (short power_downs), leaving longer power_downs and other types intact.
func TestTSFilePurgeEvents(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	events := []DownEvent{
		{Type: "power_down", Start: 100, End: 220, DurationSec: 120},           // short restart artifact
		{Type: "power_down", Start: 400, End: 460, DurationSec: 60},            // short restart artifact
		{Type: "power_down", Start: 1000, End: 1000 + 3600, DurationSec: 3600}, // real outage
		{Type: "net_down", Start: 5000, End: 5100, DurationSec: 100},           // different type
	}
	for _, e := range events {
		if err := s.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}

	// Purge power_downs shorter than 300s.
	keep := func(e DownEvent) bool {
		return !(e.Type == "power_down" && e.DurationSec < 300)
	}
	removed, err := s.PurgeEvents(keep)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 short power_downs", removed)
	}

	evs, err := s.Events(0, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("after purge = %+v, want 2 survivors (the long power_down + net_down)", evs)
	}
	for _, e := range evs {
		if e.Type == "power_down" && e.DurationSec < 300 {
			t.Errorf("a short power_down survived the purge: %+v", e)
		}
	}
}

func TestTSFilePrune(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	const day = int64(86400)
	now := int64(60 * day)

	if err := s.Append(now-40*day, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(now-1*day, MetricSet{"cpu": 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(DownEvent{Type: "power_down", Start: now - 40*day, End: now - 40*day, DurationSec: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(DownEvent{Type: "power_down", Start: now - day, End: now - day, DurationSec: 1}); err != nil {
		t.Fatal(err)
	}

	rawPath := filepath.Join(dir, "ts", "raw", "cpu.tsd")
	fiBefore, err := os.Stat(rawPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}

	fiAfter, err := os.Stat(rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if fiAfter.Size() >= fiBefore.Size() {
		t.Fatalf("raw file did not shrink: before=%d after=%d", fiBefore.Size(), fiAfter.Size())
	}

	pts, err := s.Query("cpu", 0, now, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].TS != now-1*day {
		t.Fatalf("after prune points = %+v, want just the recent one", pts)
	}

	evs, err := s.Events(0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Start != now-day {
		t.Fatalf("after prune events = %+v, want just the recent one", evs)
	}

	// File must still be valid (header intact) after prune, proving the
	// temp+rename swap didn't corrupt it.
	if err := s.Append(now, MetricSet{"cpu": 3}); err != nil {
		t.Fatalf("append after prune failed, file likely corrupted: %v", err)
	}
}

// TestTSFilePruneReleasesLockBetweenFiles pins the #113 fix: Prune must take the store
// write lock PER FILE, not once for the whole pass.
func TestTSFilePruneReleasesLockBetweenFiles(t *testing.T) {
	dir := t.TempDir()
	// Concrete *tsFileStore (not the SampleStore interface) so the test can
	// probe s.mu directly.
	s, err := newTSFileStore(dir, StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const day = int64(86400)
	now := int64(60 * day)
	// Several distinct series so pruneDir iterates multiple raw files.
	if err := s.Append(now-40*day, MetricSet{"cpu": 1, "mem": 1, "disk": 1, "temp": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(now-1*day, MetricSet{"cpu": 2, "mem": 2, "disk": 2, "temp": 2}); err != nil {
		t.Fatal(err)
	}

	var calls, acquired int
	pruneBetweenFilesHook = func() {
		calls++
		// This runs BETWEEN per-file prunes; the store lock must be free here.
		if s.mu.TryLock() {
			acquired++
			s.mu.Unlock()
		}
	}
	defer func() { pruneBetweenFilesHook = nil }()

	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("between-files hook never fired; expected several raw series to iterate")
	}
	if acquired != calls {
		t.Fatalf("store lock was held on %d of %d between-file checks: Prune still pins the lock across the pass (#113)", calls-acquired, calls)
	}
}

// A series whose every sample has aged out past retention must be DELETED by Prune, not
// rewritten as an empty file -- otherwise dead targets.
func TestTSFilePruneReapsDeadSeries(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	const day = int64(86400)
	now := int64(60 * day)

	// "dead": only data older than raw retention. "live": a recent point.
	if err := s.Append(now-40*day, MetricSet{"dead": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(now-1*day, MetricSet{"live": 2}); err != nil {
		t.Fatal(err)
	}

	deadPath := filepath.Join(dir, "ts", "raw", "dead.tsd")
	livePath := filepath.Join(dir, "ts", "raw", "live.tsd")
	if _, err := os.Stat(deadPath); err != nil {
		t.Fatalf("dead series file should exist before prune: %v", err)
	}

	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}

	// The dead series file must be gone, not an empty leftover.
	if _, err := os.Stat(deadPath); !os.IsNotExist(err) {
		t.Fatalf("dead series should have been reaped (deleted); stat err = %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Fatalf("live series file should remain: %v", err)
	}
	pts, err := s.Query("live", 0, now, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].TS != now-1*day {
		t.Fatalf("live points = %+v, want just the recent one", pts)
	}
	dpts, err := s.Query("dead", 0, now, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(dpts) != 0 {
		t.Fatalf("dead series should have no points after reap, got %+v", dpts)
	}
	// Cardinality now reflects only the live series.
	sc, _, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if sc != 1 {
		t.Fatalf("seriesCount = %d after reaping the dead series, want 1", sc)
	}
}

func TestTSFileSafeMetricDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	metrics := []string{"docker:web", "disk:/", "net:eth0:rx"}
	for i, m := range metrics {
		if err := s.Append(int64(i+1), MetricSet{m: float64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	files, err := filepath.Glob(filepath.Join(dir, "ts", "raw", "*.tsd"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(metrics) {
		t.Fatalf("expected %d distinct files, got %v", len(metrics), files)
	}

	for i, m := range metrics {
		pts, err := s.Query(m, 0, 1000, ResRaw)
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) != 1 || pts[0].Avg != float64(i+1) {
			t.Fatalf("metric %q points = %+v, want single point %v", m, pts, i+1)
		}
	}
}

func TestTSFileConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := int64(i + 1)
			metric := "cpu"
			if i%2 == 0 {
				metric = "mem"
			}
			if err := s.Append(ts, MetricSet{metric: float64(i)}); err != nil {
				t.Error(err)
			}
			if err := s.AppendEvent(DownEvent{Type: "net_down", Start: ts, End: ts, DurationSec: 0}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	cpuPts, err := s.Query("cpu", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	memPts, err := s.Query("mem", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(cpuPts)+len(memPts) != 50 {
		t.Fatalf("concurrent append: got %d cpu + %d mem = %d points, want 50", len(cpuPts), len(memPts), len(cpuPts)+len(memPts))
	}

	evs, err := s.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 50 {
		t.Fatalf("concurrent append: got %d events, want 50", len(evs))
	}
}

func TestSafeMetricInjective(t *testing.T) {
	// Previously-colliding pairs must now map to distinct filenames.
	pairs := [][2]string{
		{"a/b", "a_b"},
		{"a:b", "a b"},
		{"disk:/mnt/my disk", "disk:/mnt/my/disk"},
	}
	for _, p := range pairs {
		s1, s2 := safeMetric(p[0]), safeMetric(p[1])
		if s1 == s2 {
			t.Fatalf("safeMetric collision: %q and %q both -> %q", p[0], p[1], s1)
		}
	}

	// And each must round-trip independently via Append/Query (distinct files,
	// no interleaving).
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	all := []string{"a/b", "a_b", "a:b", "a b", "disk:/mnt/my disk", "disk:/mnt/my/disk"}
	for i, m := range all {
		if err := s.Append(int64(i+1), MetricSet{m: float64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	for i, m := range all {
		pts, err := s.Query(m, 0, 1000, ResRaw)
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) != 1 || pts[0].Avg != float64(i+1) {
			t.Fatalf("metric %q = %+v, want exactly one point %v (no cross-series interleaving)", m, pts, i+1)
		}
	}
}

func TestTSFileDownsampleRollsCompletedMinutes(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	// 3 minute-buckets (0, 60, 120), several distinct values per minute so
	// Min/Avg/Max differ from a single repeated value.
	appends := []struct {
		ts int64
		v  float64
	}{
		{0, 10}, {20, 20}, {40, 30}, // minute 0: min=10 avg=20 max=30
		{60, 5}, {90, 15}, // minute 1: min=5 avg=10 max=15
		{120, 100}, {150, 200}, // minute 2: min=100 avg=150 max=200
	}
	for _, a := range appends {
		if err := s.Append(a.ts, MetricSet{"cpu": a.v}); err != nil {
			t.Fatal(err)
		}
	}

	// now=180 is exactly the end of minute-bucket 120 (120+60=180<=180), so
	// all three buckets are fully in the past.
	now := int64(180)
	if err := s.Downsample(now); err != nil {
		t.Fatal(err)
	}

	pts, err := s.Query("cpu", 0, 1000, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("1m points = %+v, want 3 completed minute buckets", pts)
	}
	want := []Point{
		{TS: 0, Min: 10, Avg: 20, Max: 30},
		{TS: 60, Min: 5, Avg: 10, Max: 15},
		{TS: 120, Min: 100, Avg: 150, Max: 200},
	}
	for i, p := range pts {
		w := want[i]
		if p.TS != w.TS || p.Min != w.Min || p.Avg != w.Avg || p.Max != w.Max {
			t.Fatalf("bucket %d = %+v, want %+v", i, p, w)
		}
	}

	// Idempotent: a second Downsample call must not duplicate any bucket.
	if err := s.Downsample(now); err != nil {
		t.Fatal(err)
	}
	pts2, err := s.Query("cpu", 0, 1000, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts2) != 3 {
		t.Fatalf("after second Downsample, 1m points = %+v, want still 3 (no duplicates)", pts2)
	}
}

func TestTSFileDownsampleSkipsIncompleteCurrentMinute(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	if err := s.Append(0, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(30, MetricSet{"cpu": 2}); err != nil {
		t.Fatal(err)
	}

	// now=30 falls inside minute-bucket 0 (ends at 60): not yet complete.
	if err := s.Downsample(30); err != nil {
		t.Fatal(err)
	}
	pts, err := s.Query("cpu", 0, 1000, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 0 {
		t.Fatalf("1m points = %+v, want none (current minute incomplete)", pts)
	}

	// Once now reaches the bucket's end, it becomes eligible.
	if err := s.Downsample(60); err != nil {
		t.Fatal(err)
	}
	pts, err = s.Query("cpu", 0, 1000, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].TS != 0 {
		t.Fatalf("1m points = %+v, want one bucket at ts=0", pts)
	}
}

func TestTSFilePerResolutionRetention(t *testing.T) {
	dir := t.TempDir()
	opts := StoreOptions{RawRetention: 2 * time.Hour, RollupRetention: 24 * time.Hour, EventRetention: 24 * time.Hour}
	s := openTSFile(t, dir, opts)
	defer s.Close()

	const hour = int64(3600)
	now := int64(48 * hour)

	// Raw: one point older than RawRetention (2h), one within it.
	if err := s.Append(now-3*hour, MetricSet{"cpu": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(now-1*hour, MetricSet{"cpu": 2}); err != nil {
		t.Fatal(err)
	}

	// 1m: one bucket older than RollupRetention (24h), one within it but outside RawRetention
	// (2h).
	oldBucket := ((now - 30*hour) / 60) * 60
	recentBucket := ((now - 10*hour) / 60) * 60
	if err := s.Append(oldBucket, MetricSet{"mem": 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(recentBucket, MetricSet{"mem": 6}); err != nil {
		t.Fatal(err)
	}
	if err := s.Downsample(now); err != nil {
		t.Fatal(err)
	}

	if err := s.Prune(now); err != nil {
		t.Fatal(err)
	}

	rawPts, err := s.Query("cpu", 0, now, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rawPts) != 1 || rawPts[0].TS != now-1*hour {
		t.Fatalf("raw after prune = %+v, want just the point within RawRetention (2h)", rawPts)
	}

	rollupPts, err := s.Query("mem", 0, now, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(rollupPts) != 1 || rollupPts[0].TS != recentBucket {
		t.Fatalf("1m after prune = %+v, want just the bucket within RollupRetention (24h)", rollupPts)
	}
}

func TestSafeMetricDeterministicAndDistinct(t *testing.T) {
	cases := []string{"docker:web", "disk:/", "net:eth0:rx", "cpu", ""}
	seen := map[string]string{}
	for _, m := range cases {
		safe := safeMetric(m)
		if safe == "" {
			t.Fatalf("safeMetric(%q) returned empty string", m)
		}
		if safe2 := safeMetric(m); safe2 != safe {
			t.Fatalf("safeMetric(%q) not deterministic: %q vs %q", m, safe, safe2)
		}
		if other, ok := seen[safe]; ok && other != m {
			t.Fatalf("safeMetric collision: %q and %q both map to %q", m, other, safe)
		}
		seen[safe] = m
	}
}

// TestTSFileStoreStats asserts tsFileStore.Stats counts the .tsd files written under
// ts/raw.
func TestTSFileStoreStats(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir, StoreOptions{})
	defer s.Close()

	if err := s.Append(100, MetricSet{"cpu": 10, "mem": 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(200, MetricSet{"cpu": 20, "mem": 60}); err != nil {
		t.Fatal(err)
	}

	n, size, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("seriesCount = %d, want >= 2 (cpu.tsd, mem.tsd)", n)
	}
	if size <= 0 {
		t.Errorf("diskBytes = %d, want > 0", size)
	}

	// AppendEvent adds one more file (events.tsd) to the count/size.
	if err := s.AppendEvent(DownEvent{Type: "power_down", Start: 1, End: 2, DurationSec: 1}); err != nil {
		t.Fatal(err)
	}
	n2, size2, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n2 != n+1 {
		t.Errorf("seriesCount after AppendEvent = %d, want %d (+1 for events.tsd)", n2, n+1)
	}
	if size2 <= size {
		t.Errorf("diskBytes after AppendEvent = %d, want > %d", size2, size)
	}
}

// TestTSFileStoreStatsEmptyDir asserts Stats on a freshly-opened store (no Append yet)
// reports zero series and zero bytes rather than erroring.
func TestTSFileStoreStatsEmptyDir(t *testing.T) {
	s := openTSFile(t, t.TempDir(), StoreOptions{})
	defer s.Close()

	n, size, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || size != 0 {
		t.Errorf("Stats() on empty store = (%d, %d), want (0, 0)", n, size)
	}
}
