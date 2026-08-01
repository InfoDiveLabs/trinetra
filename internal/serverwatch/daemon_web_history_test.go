//go:build web

package serverwatch

import (
	"errors"
	"testing"

	"serverwatch/internal/config"
)

// fakeSampleStoreQuery is a minimal SampleStore test double for
// seriesStoreAdapter. Only Events is exercised by the tests below (the
// still-live production path, via eventsStoreFor); Query stays implemented
// purely to satisfy the SampleStore interface, and every other method is a
// harmless no-op/zero-value stub. gotRes/gotFrom/gotTo are populated by
// Query but no longer read by any test.
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

// TestEventsStoreForNilStoreYieldsNilInterface pins the classic Go
// nil-interface gotcha this wiring must avoid: nil store -> true nil
// web.EventsStore, not a non-nil interface wrapping a nil *seriesStoreAdapter
// pointer.
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
