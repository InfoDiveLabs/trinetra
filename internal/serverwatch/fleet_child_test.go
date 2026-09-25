package serverwatch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"serverwatch/internal/fleet"
)

type captureTee struct {
	samples chan MetricSet
	events  chan DownEvent
}

func (c *captureTee) Samples(ts int64, ms MetricSet) { c.samples <- ms }
func (c *captureTee) Event(e DownEvent)              { c.events <- e }

func TestStoreWriterTeesAfterLocalWrite(t *testing.T) {
	store := newMemStore(StoreOptions{})
	w := newStoreWriter(store, 4)
	ct := &captureTee{samples: make(chan MetricSet, 4), events: make(chan DownEvent, 4)}
	w.setTee(ct)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.run(ctx)
	w.submit(storeWrite{ts: 10, sets: []MetricSet{{"cpu": 5}}, events: []DownEvent{{Type: "net_down", Start: 1, End: 2}}})
	select {
	case ms := <-ct.samples:
		if ms["cpu"] != 5 {
			t.Fatalf("teed %v", ms)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no teed samples")
	}
	<-ct.events
	if pts, _ := store.Query("cpu", 0, 100, ResRaw); len(pts) != 1 {
		t.Fatal("local write missing")
	}
}

func TestAlertLogTee(t *testing.T) {
	l := NewAlertLog(filepath.Join(t.TempDir(), "alertlog.jsonl"))
	var got []AlertEvent
	l.SetTee(func(ev AlertEvent) { got = append(got, ev) })
	if err := l.AppendAlertEvent(AlertEvent{Time: 5, Key: "cpu"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "cpu" {
		t.Fatalf("tee got %+v", got)
	}
}

func TestOutboxTeeAppendsAllKinds(t *testing.T) {
	ob, _ := fleet.OpenOutbox(t.TempDir(), 64<<20)
	defer ob.Close()
	tee := newOutboxTee(ob, t.Logf)
	tee.Samples(100, MetricSet{"cpu": 1})
	tee.Event(DownEvent{Type: "power_down", Start: 50, End: 60, DurationSec: 10})
	tee.Alert(AlertEvent{Time: 101, Key: "cpu"})
	recs, _ := ob.Read(0, 1<<20, 10)
	if len(recs) != 3 || recs[0].Kind != fleet.KindSamples || recs[1].Kind != fleet.KindDownEvent || recs[2].Kind != fleet.KindAlert || recs[1].TS != 50 {
		t.Fatalf("records = %+v", recs)
	}
}

func TestLocalGapFillerUsesRollupThenRaw(t *testing.T) {
	store, _ := newTSFileStore(t.TempDir(), StoreOptions{RawRetention: time.Hour})
	now := time.Unix(1_000_000, 0)
	old := now.Add(-3 * time.Hour).Unix()
	for i := int64(0); i < 3; i++ {
		store.AppendRollup("cpu", Point{TS: old + i*60, Min: 1, Avg: 2, Max: 3})
	}
	recent := now.Add(-10 * time.Minute).Unix()
	for i := int64(0); i < 3; i++ {
		store.Append(recent+i*5, MetricSet{"cpu": float64(i)})
	}
	store.AppendEvent(DownEvent{Type: "net_down", Start: old + 30, End: old + 90, DurationSec: 60})
	alog := NewAlertLog(filepath.Join(t.TempDir(), "a.jsonl"))
	alog.AppendAlertEvent(AlertEvent{Time: recent + 1, Key: "cpu"})

	g := &localGapFiller{store: store, alog: alog, rawRetention: time.Hour, now: func() time.Time { return now }}
	recs, err := g.Fill(fleet.Gap{FirstSeq: 1, LastSeq: 99, MinTS: old, MaxTS: recent + 10})
	if err != nil {
		t.Fatal(err)
	}
	var rollups, raws, events, alerts int
	lastTS := int64(0)
	for _, r := range recs {
		if r.TS < lastTS {
			t.Fatalf("records not in ts order at %d", r.TS)
		}
		lastTS = r.TS
		switch r.Kind {
		case fleet.KindSamples:
			var d fleet.SamplesData
			json.Unmarshal(r.Data, &d)
			if d.Res == "1m" {
				rollups++
			} else {
				raws++
			}
		case fleet.KindDownEvent:
			events++
		case fleet.KindAlert:
			alerts++
		}
	}
	if rollups != 3 || raws != 3 || events != 1 || alerts != 1 {
		t.Fatalf("rollups=%d raws=%d events=%d alerts=%d", rollups, raws, events, alerts)
	}
}

func TestLiveBuilderIncludesHostInfoPeriodically(t *testing.T) {
	lb := newLiveBuilder(func() Snapshot { return Snapshot{CPU: 3} }, filepath.Join(t.TempDir(), "missing.json"), func() HostInfo { return HostInfo{Hostname: "h"} })
	u1, _ := lb.Build()
	u2, _ := lb.Build()
	if len(u1.HostInfo) == 0 || len(u2.HostInfo) != 0 || !strings.Contains(string(u1.Snapshot), `"cpu":3`) {
		t.Fatalf("u1=%s/%s u2 hostinfo=%s", u1.Snapshot, u1.HostInfo, u2.HostInfo)
	}
}

func TestChildLinkAlerts(t *testing.T) {
	var c childLinkAlerts
	start := int64(1000)
	if a := c.Plan(fleet.LinkStatus{State: "retrying"}, "https://m", start, start+300); len(a) != 0 {
		t.Fatalf("alert before 10m: %+v", a)
	}
	a := c.Plan(fleet.LinkStatus{State: "retrying"}, "https://m", start, start+601)
	if len(a) != 1 || a[0].Severity != SevWarning || a[0].Kind != "fire" {
		t.Fatalf("link down alert = %+v", a)
	}
	if again := c.Plan(fleet.LinkStatus{State: "retrying"}, "https://m", start, start+700); len(again) != 0 {
		t.Fatal("link down alert repeated")
	}
	a = c.Plan(fleet.LinkStatus{State: "linked", LastAck: start + 710}, "https://m", start, start+710)
	if len(a) != 1 || a[0].Kind != "recover" {
		t.Fatalf("recovery = %+v", a)
	}
	a = c.Plan(fleet.LinkStatus{State: "revoked"}, "https://m", start, start+800)
	if len(a) != 1 || a[0].Severity != SevCritical {
		t.Fatalf("revoked = %+v", a)
	}
	if again := c.Plan(fleet.LinkStatus{State: "revoked"}, "https://m", start, start+900); len(again) != 0 {
		t.Fatal("revoked alert repeated")
	}
}

// "catching up" means the master is reachable again (live updates get
// through while the backlog drains): it resolves the link-down warning and
// never raises one.
func TestChildLinkAlertsCatchingUpIsReachable(t *testing.T) {
	var c childLinkAlerts
	start := int64(1000)
	if a := c.Plan(fleet.LinkStatus{State: "retrying"}, "https://m", start, start+601); len(a) != 1 || a[0].Kind != "fire" {
		t.Fatalf("link down alert = %+v", a)
	}
	a := c.Plan(fleet.LinkStatus{State: fleet.LinkCatchingUp, LastAck: start + 700}, "https://m", start, start+700)
	if len(a) != 1 || a[0].Kind != "recover" {
		t.Fatalf("catching up should recover the link alert, got %+v", a)
	}
	var d childLinkAlerts
	if a := d.Plan(fleet.LinkStatus{State: fleet.LinkCatchingUp}, "https://m", start, start+601); len(a) != 0 {
		t.Fatalf("catching up raised %+v", a)
	}
}
