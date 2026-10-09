package trinetra

import (
	"sync"
	"testing"
	"time"
)

func TestMemStoreAppendQuery(t *testing.T) {
	s, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

	// 1m resolution: memory doesn't downsample, returns same raw points
	pts, err = s.Query("cpu", 0, 1000, Res1m)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("cpu 1m points = %+v, want 3 (memory does not downsample)", pts)
	}
}

func TestMemStoreEventsRoundTrip(t *testing.T) {
	s, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
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

	// range filter: only overlapping [from,to] events by End>=from && Start<=to
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

func TestMemStorePrune(t *testing.T) {
	s, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const day = int64(86400)
	now := int64(60 * day) // well past the 30d retention window

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

	if err := s.Prune(now); err != nil {
		t.Fatal(err)
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
}

func TestMemStoreConcurrentAppend(t *testing.T) {
	s, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ts := int64(i + 1)
			if err := s.Append(ts, MetricSet{"cpu": float64(i)}); err != nil {
				t.Error(err)
			}
			if err := s.AppendEvent(DownEvent{Type: "net_down", Start: ts, End: ts, DurationSec: 0}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	pts, err := s.Query("cpu", 0, 1000, ResRaw)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 50 {
		t.Fatalf("concurrent append: got %d points, want 50", len(pts))
	}
	evs, err := s.Events(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 50 {
		t.Fatalf("concurrent append: got %d events, want 50", len(evs))
	}
}

func TestOpenStoreBackendSelection(t *testing.T) {
	if s, err := OpenStore("memory", t.TempDir(), StoreOptions{}); err != nil {
		t.Fatalf("memory backend: %v", err)
	} else {
		defer s.Close()
	}

	if s, err := OpenStore("tsfile", t.TempDir(), StoreOptions{}); err != nil {
		t.Fatalf("tsfile backend: %v", err)
	} else {
		defer s.Close()
	}

	if _, err := OpenStore("bogus", t.TempDir(), StoreOptions{}); err == nil {
		t.Fatal("bogus backend: want error, got nil")
	}
}

// TestOpenStoreOptionsPlumbed asserts that a StoreOptions passed to OpenStore actually
// governs Prune for both backends: a custom.
func TestOpenStoreOptionsPlumbed(t *testing.T) {
	const day = int64(86400)
	now := int64(60 * day)
	opts := StoreOptions{RawRetention: time.Hour, RollupRetention: time.Hour, EventRetention: time.Hour}

	for _, backend := range []string{"memory", "tsfile"} {
		t.Run(backend, func(t *testing.T) {
			s, err := OpenStore(backend, t.TempDir(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			if err := s.Append(now-2*day, MetricSet{"cpu": 1}); err != nil {
				t.Fatal(err)
			}
			if err := s.Append(now-1, MetricSet{"cpu": 2}); err != nil {
				t.Fatal(err)
			}
			if err := s.Prune(now); err != nil {
				t.Fatal(err)
			}
			pts, err := s.Query("cpu", 0, now, ResRaw)
			if err != nil {
				t.Fatal(err)
			}
			if len(pts) != 1 || pts[0].TS != now-1 {
				t.Fatalf("%s: after prune with 1h retention, points = %+v, want just the recent one", backend, pts)
			}
		})
	}
}

// TestMemStoreStats asserts memStore.Stats reports the number of in-memory
// metric series and always 0 disk bytes (it is non-persistent).
func TestMemStoreStats(t *testing.T) {
	s, err := OpenStore("memory", t.TempDir(), StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.Append(100, MetricSet{"cpu": 10, "mem": 50}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(200, MetricSet{"cpu": 20}); err != nil {
		t.Fatal(err)
	}

	n, bytes, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("seriesCount = %d, want 2 (cpu, mem)", n)
	}
	if bytes != 0 {
		t.Errorf("diskBytes = %d, want 0 (memStore is non-persistent)", bytes)
	}
}

func TestPickResolution(t *testing.T) {
	const day = int64(86400)
	now := int64(100 * day)
	rawRetention := 48 * time.Hour

	cases := []struct {
		name string
		from int64
		want Resolution
	}{
		{"well within raw window", now - int64(time.Hour.Seconds()), ResRaw},
		{"exactly at the boundary", now - int64(rawRetention.Seconds()), ResRaw},
		{"just past the boundary", now - int64(rawRetention.Seconds()) - 1, Res1m},
		{"far in the past", now - 30*day, Res1m},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PickResolution(tc.from, now, now, rawRetention)
			if got != tc.want {
				t.Fatalf("PickResolution(from=%d, now=%d, rawRetention=%v) = %v, want %v", tc.from, now, rawRetention, got, tc.want)
			}
		})
	}
}
