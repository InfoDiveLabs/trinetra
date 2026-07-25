package serverwatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTSFile(t *testing.T, dir string) SampleStore {
	t.Helper()
	s, err := OpenStore("tsfile", dir)
	if err != nil {
		t.Fatalf("OpenStore(tsfile): %v", err)
	}
	return s
}

func TestTSFileAppendQuery(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir)
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
	s := openTSFile(t, dir)
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
	s := openTSFile(t, dir)

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

	s2 := openTSFile(t, dir)
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
	s := openTSFile(t, dir)
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
	s := openTSFile(t, dir)
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
	s := openTSFile(t, dir)
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

func TestTSFilePrune(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir)
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

func TestTSFileSafeMetricDistinctFiles(t *testing.T) {
	dir := t.TempDir()
	s := openTSFile(t, dir)
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
	s := openTSFile(t, dir)
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
