package pluginhost

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/InfoDiveLabs/trinetra/internal/plugin"
)

type sink struct {
	ts  []int64
	got []map[string]float64
	ok  bool
}

func (s *sink) submit(ts int64, ms map[string]float64) bool {
	if !s.ok {
		return false
	}
	s.ts = append(s.ts, ts)
	s.got = append(s.got, ms)
	return true
}

var fixedNow = time.Unix(1_800_000_000, 0)

func newGate(t *testing.T, dir string, s *sink) *SeriesGate {
	return NewSeriesGate(dir, s.submit, func() time.Time { return fixedNow })
}

func TestPublishPrefixesNamespace(t *testing.T) {
	s := &sink{ok: true}
	g := newGate(t, t.TempDir(), s)
	resp := g.Publish("probe", plugin.PublishRequest{Metrics: map[string]float64{"latency_ms": 5}})
	if resp.Accepted != 1 || len(resp.Rejected) != 0 || resp.Dropped {
		t.Fatalf("%+v", resp)
	}
	if s.got[0]["plugin.probe.latency_ms"] != 5 || s.ts[0] != fixedNow.Unix() {
		t.Fatalf("%v %v", s.got, s.ts)
	}
}

func TestPublishRejectsBadNames(t *testing.T) {
	s := &sink{ok: true}
	g := newGate(t, t.TempDir(), s)
	m := map[string]float64{"Upper": 1, "a/b": 1, "": 1, strings.Repeat("a", 97): 1, "good": 2}
	resp := g.Publish("probe", plugin.PublishRequest{Metrics: m})
	if resp.Accepted != 1 || len(resp.Rejected) != 4 {
		t.Fatalf("%+v", resp)
	}
	if g.Rejected("probe") != 4 {
		t.Fatalf("rejected %d", g.Rejected("probe"))
	}
}

func TestPublishCapsDistinctSeriesAt200(t *testing.T) {
	s := &sink{ok: true}
	g := newGate(t, t.TempDir(), s)
	m := map[string]float64{}
	for i := 0; i < 200; i++ {
		m[fmt.Sprintf("m%03d", i)] = 1
	}
	if r := g.Publish("probe", plugin.PublishRequest{Metrics: m}); r.Accepted != 200 {
		t.Fatalf("%+v", r)
	}
	r := g.Publish("probe", plugin.PublishRequest{Metrics: map[string]float64{"extra": 1, "m001": 2}})
	if r.Accepted != 1 || len(r.Rejected) != 1 || r.Rejected[0] != "extra" {
		t.Fatalf("%+v", r)
	}
	if g.Count("probe") != 200 {
		t.Fatalf("count %d", g.Count("probe"))
	}
}

func TestPublishCapSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := &sink{ok: true}
	g := newGate(t, dir, s)
	m := map[string]float64{}
	for i := 0; i < 200; i++ {
		m[fmt.Sprintf("m%03d", i)] = 1
	}
	g.Publish("probe", plugin.PublishRequest{Metrics: m})
	g2 := newGate(t, dir, s)
	if g2.Count("probe") != 200 {
		t.Fatalf("count after restart %d", g2.Count("probe"))
	}
	r := g2.Publish("probe", plugin.PublishRequest{Metrics: map[string]float64{"brand_new": 1}})
	if r.Accepted != 0 || len(r.Rejected) != 1 {
		t.Fatalf("%+v", r)
	}
	g2.Forget("probe")
	if newGate(t, dir, s).Count("probe") != 0 {
		t.Fatal("forget did not clear")
	}
}

func TestPublishRejectsNaNInfAndSkewedTS(t *testing.T) {
	s := &sink{ok: true}
	g := newGate(t, t.TempDir(), s)
	r := g.Publish("probe", plugin.PublishRequest{Metrics: map[string]float64{"a": math.NaN(), "b": math.Inf(1), "c": math.Inf(-1), "d": 1}})
	if r.Accepted != 1 || len(r.Rejected) != 3 {
		t.Fatalf("%+v", r)
	}
	for _, ts := range []int64{fixedNow.Unix() - 601, fixedNow.Unix() + 61, 1} {
		r := g.Publish("probe", plugin.PublishRequest{TS: ts, Metrics: map[string]float64{"x": 1, "y": 2}})
		if r.Accepted != 0 || len(r.Rejected) != 2 {
			t.Fatalf("ts %d: %+v", ts, r)
		}
	}
	for _, ts := range []int64{fixedNow.Unix() - 600, fixedNow.Unix() + 60} {
		if r := g.Publish("probe", plugin.PublishRequest{TS: ts, Metrics: map[string]float64{"x": 1}}); r.Accepted != 1 {
			t.Fatalf("ts %d: %+v", ts, r)
		}
	}
}

func TestPublishRejectsHugeBatch(t *testing.T) {
	s := &sink{ok: true}
	g := newGate(t, t.TempDir(), s)
	m := map[string]float64{}
	for i := 0; i < 10_000; i++ {
		m[fmt.Sprintf("m%d", i)] = 1
	}
	r := g.Publish("probe", plugin.PublishRequest{Metrics: m})
	if r.Accepted != 0 || len(r.Rejected) != 20 || len(s.got) != 0 {
		t.Fatalf("accepted=%d rejected=%d", r.Accepted, len(r.Rejected))
	}
	if g.Rejected("probe") != 10_000 || g.Count("probe") != 0 {
		t.Fatalf("rej=%d count=%d", g.Rejected("probe"), g.Count("probe"))
	}
}

func TestPublishDropOnFullReturnsDropped(t *testing.T) {
	s := &sink{ok: false}
	g := newGate(t, t.TempDir(), s)
	start := time.Now()
	r := g.Publish("probe", plugin.PublishRequest{Metrics: map[string]float64{"a": 1}})
	if !r.Dropped || r.Accepted != 0 {
		t.Fatalf("%+v", r)
	}
	if time.Since(start) > 10*time.Millisecond {
		t.Fatal("slow")
	}
	if g.Count("probe") != 0 {
		t.Fatal("dropped name consumed cap")
	}
}
