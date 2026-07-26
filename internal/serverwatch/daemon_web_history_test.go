//go:build web

package serverwatch

import (
	"errors"
	"testing"
	"time"

	"serverwatch/internal/config"
)

// fakeSampleStoreQuery is a minimal SampleStore test double for
// seriesStoreAdapter: only Query is exercised, so every other method is a
// harmless no-op/zero-value stub. gotRes/gotFrom/gotTo record the last
// Query call's arguments so tests can pin exactly what resolution the
// adapter picked.
type fakeSampleStoreQuery struct {
	pts    []Point
	err    error
	events []DownEvent
	evErr  error

	gotMetric          string
	gotFrom, gotTo     int64
	gotRes             Resolution
	gotEvFrom, gotEvTo int64
}

func (f *fakeSampleStoreQuery) Append(ts int64, m MetricSet) error { return nil }
func (f *fakeSampleStoreQuery) Query(metric string, from, to int64, res Resolution) ([]Point, error) {
	f.gotMetric, f.gotFrom, f.gotTo, f.gotRes = metric, from, to, res
	if f.err != nil {
		return nil, f.err
	}
	return f.pts, nil
}
func (f *fakeSampleStoreQuery) AppendEvent(e DownEvent) error { return nil }
func (f *fakeSampleStoreQuery) Events(from, to int64) ([]DownEvent, error) {
	f.gotEvFrom, f.gotEvTo = from, to
	if f.evErr != nil {
		return nil, f.evErr
	}
	return f.events, nil
}
func (f *fakeSampleStoreQuery) Prune(nowUnix int64) error      { return nil }
func (f *fakeSampleStoreQuery) Downsample(nowUnix int64) error { return nil }
func (f *fakeSampleStoreQuery) Close() error                   { return nil }
func (f *fakeSampleStoreQuery) Stats() (int, int64, error)     { return 0, 0, nil }

// TestSeriesStoreAdapterConvertsPoints pins the field-by-field Point ->
// web.SeriesPoint conversion (the same shape widening buildDashboardView
// does for Snapshot -> DashboardView).
func TestSeriesStoreAdapterConvertsPoints(t *testing.T) {
	fake := &fakeSampleStoreQuery{pts: []Point{
		{TS: 1000, Min: 1, Avg: 2, Max: 3},
		{TS: 2000, Min: 4, Avg: 5, Max: 6},
	}}
	adapter := &seriesStoreAdapter{store: fake, cfg: func() *config.Config { return config.Default() }}

	got, err := adapter.Query("cpu", 500, 2500)
	if err != nil {
		t.Fatalf("Query error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].TS != 1000 || got[0].Min != 1 || got[0].Avg != 2 || got[0].Max != 3 {
		t.Errorf("got[0] = %+v, want {TS:1000 Min:1 Avg:2 Max:3}", got[0])
	}
	if got[1].TS != 2000 || got[1].Min != 4 || got[1].Avg != 5 || got[1].Max != 6 {
		t.Errorf("got[1] = %+v, want {TS:2000 Min:4 Avg:5 Max:6}", got[1])
	}
	if fake.gotMetric != "cpu" || fake.gotFrom != 500 || fake.gotTo != 2500 {
		t.Errorf("underlying Query called with (%q, %d, %d), want (cpu, 500, 2500)", fake.gotMetric, fake.gotFrom, fake.gotTo)
	}
}

// TestSeriesStoreAdapterPicksResolutionFromConfiguredRawRetention pins that
// the adapter resolves raw-vs-1m internally (via PickResolution and the
// daemon's configured storage.raw_retention) rather than leaving that
// decision to internal/web, which has no Resolution type at all.
func TestSeriesStoreAdapterPicksResolutionFromConfiguredRawRetention(t *testing.T) {
	fake := &fakeSampleStoreQuery{}
	cfg := config.Default()
	cfg.Storage.RawRetention = "48h"
	adapter := &seriesStoreAdapter{store: fake, cfg: func() *config.Config { return cfg }}

	now := time.Now().Unix()

	// A range starting well within the last 48h -> raw.
	if _, err := adapter.Query("cpu", now-3600, now); err != nil {
		t.Fatalf("Query error: %v", err)
	}
	if fake.gotRes != ResRaw {
		t.Errorf("recent range resolved to %v, want ResRaw", fake.gotRes)
	}

	// A range starting 30 days back -> past the 48h raw window -> 1m.
	if _, err := adapter.Query("cpu", now-30*86400, now-29*86400); err != nil {
		t.Fatalf("Query error: %v", err)
	}
	if fake.gotRes != Res1m {
		t.Errorf("old range resolved to %v, want Res1m", fake.gotRes)
	}
}

// TestSeriesStoreAdapterPropagatesStoreError pins that a real Query error
// from the underlying SampleStore is propagated (not swallowed) — it's
// internal/web's job (seriesAPIHandler) to decide an error still renders as
// an empty 200 response, not this adapter's.
func TestSeriesStoreAdapterPropagatesStoreError(t *testing.T) {
	wantErr := errors.New("boom")
	fake := &fakeSampleStoreQuery{err: wantErr}
	adapter := &seriesStoreAdapter{store: fake, cfg: func() *config.Config { return config.Default() }}

	_, err := adapter.Query("cpu", 1, 100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestSeriesStoreForNilStoreYieldsNilInterface pins the classic Go
// nil-interface gotcha this wiring must avoid: given a nil SampleStore,
// seriesStoreFor must return a true nil web.SeriesStore interface value —
// not a non-nil interface wrapping a nil *seriesStoreAdapter pointer, which
// would make internal/web's `d.Store != nil` nil-check
// (handlers_history.go) wrongly treat "no store configured" as "a store is
// configured" and panic dereferencing the nil adapter's fields on the first
// Query call.
func TestSeriesStoreForNilStoreYieldsNilInterface(t *testing.T) {
	got := seriesStoreFor(nil, func() *config.Config { return config.Default() })
	if got != nil {
		t.Fatalf("seriesStoreFor(nil, ...) = %#v, want a true nil interface", got)
	}
}

// TestSeriesStoreForNonNilStoreYieldsWorkingAdapter is the positive
// counterpart: a non-nil SampleStore yields a non-nil web.SeriesStore that
// actually delegates Query.
func TestSeriesStoreForNonNilStoreYieldsWorkingAdapter(t *testing.T) {
	fake := &fakeSampleStoreQuery{pts: []Point{{TS: 1, Avg: 2}}}
	got := seriesStoreFor(fake, func() *config.Config { return config.Default() })
	if got == nil {
		t.Fatal("seriesStoreFor(non-nil, ...) returned a nil interface")
	}
	pts, err := got.Query("cpu", 0, 100)
	if err != nil {
		t.Fatalf("Query error: %v", err)
	}
	if len(pts) != 1 || pts[0].TS != 1 || pts[0].Avg != 2 {
		t.Fatalf("Query() = %+v, want the fake's one point passed through", pts)
	}
}

// TestEventsStoreAdapterConvertsEvents pins the DownEvent -> web.DownEventView
// conversion (the downtime counterpart of the Point -> web.SeriesPoint
// widening) and that the from/to filter is passed through to the underlying
// SampleStore.Events.
func TestEventsStoreAdapterConvertsEvents(t *testing.T) {
	fake := &fakeSampleStoreQuery{events: []DownEvent{
		{Type: "power_down", Start: 1000, End: 1600, DurationSec: 600},
		{Type: "net_down", Start: 2000, End: 2060, DurationSec: 60},
	}}
	adapter := &seriesStoreAdapter{store: fake, cfg: func() *config.Config { return config.Default() }}

	got, err := adapter.Events(500, 2500)
	if err != nil {
		t.Fatalf("Events error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Type != "power_down" || got[0].Start != 1000 || got[0].End != 1600 || got[0].DurationSec != 600 {
		t.Errorf("got[0] = %+v, want power_down 1000..1600 600s", got[0])
	}
	if fake.gotEvFrom != 500 || fake.gotEvTo != 2500 {
		t.Errorf("underlying Events called with (%d, %d), want (500, 2500)", fake.gotEvFrom, fake.gotEvTo)
	}
}

// TestEventsStoreAdapterPropagatesError pins that a real Events error from
// the underlying SampleStore is propagated (not swallowed) — it's
// internal/web's job (downtimeAPIHandler) to render an error as an empty 200.
func TestEventsStoreAdapterPropagatesError(t *testing.T) {
	wantErr := errors.New("boom")
	fake := &fakeSampleStoreQuery{evErr: wantErr}
	adapter := &seriesStoreAdapter{store: fake, cfg: func() *config.Config { return config.Default() }}

	_, err := adapter.Events(1, 100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestEventsStoreForNilStoreYieldsNilInterface pins the same nil-interface
// safety for events as seriesStoreFor: nil store -> true nil web.EventsStore.
func TestEventsStoreForNilStoreYieldsNilInterface(t *testing.T) {
	if got := eventsStoreFor(nil); got != nil {
		t.Fatalf("eventsStoreFor(nil) = %#v, want a true nil interface", got)
	}
}

// TestEventsStoreForNonNilStoreYieldsWorkingAdapter is the positive
// counterpart.
func TestEventsStoreForNonNilStoreYieldsWorkingAdapter(t *testing.T) {
	fake := &fakeSampleStoreQuery{events: []DownEvent{{Type: "net_down", Start: 1, End: 2, DurationSec: 1}}}
	got := eventsStoreFor(fake)
	if got == nil {
		t.Fatal("eventsStoreFor(non-nil) returned a nil interface")
	}
	evs, err := got.Events(0, 100)
	if err != nil {
		t.Fatalf("Events error: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != "net_down" {
		t.Fatalf("Events() = %+v, want the fake's one event passed through", evs)
	}
}
